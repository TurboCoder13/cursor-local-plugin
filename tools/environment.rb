# Shared platform, path and local HTTP settings for deployment tools.
require 'fileutils'
require 'json'
require 'net/http'
require 'optparse'
require 'rbconfig'
require 'securerandom'
require 'uri'
require 'yaml'

module CursorLocal
  PROJECT = File.expand_path('..', __dir__).freeze

  def self.platform(os: RbConfig::CONFIG.fetch('host_os'), cpu: RbConfig::CONFIG.fetch('host_cpu'))
    goos = case os
    when /linux/ then 'linux'
    when /darwin/ then 'darwin'
    when /freebsd/ then 'freebsd'
    when /mswin|mingw|cygwin/ then 'windows'
    else raise 'platform is not supported by the CPA native plugin loader'
    end
    arch = case cpu
    when /\A(x86_64|amd64|x64)\z/ then 'amd64'
    when /\A(aarch64|arm64)\z/ then 'arm64'
    else raise 'a native amd64 or arm64 toolchain is required'
    end
    [goos, arch]
  end

  def self.extension(goos)
    {'darwin' => 'dylib', 'windows' => 'dll'}.fetch(goos, 'so')
  end

  class Environment
    attr_accessor :config, :binary, :url, :auth_dir, :plugins_dir, :library,
                  :dashboard, :dashboard_source, :management_key, :client_key, :port
    attr_reader :goos, :goarch

    def initialize(env: ENV, home: Dir.home, platform: CursorLocal.platform)
      @goos, @goarch = platform
      root = File.join(env.fetch('XDG_CONFIG_HOME', File.join(home, '.config')), 'cliproxyapi')
      @config = env.fetch('CLI_PROXY_CONFIG', File.join(root, 'config.yaml'))
      @binary = env.fetch('CLI_PROXY_BINARY', 'cliproxyapi')
      @url = env.fetch('CLI_PROXY_URL', 'http://127.0.0.1:8317')
      @auth_dir = env['CLI_PROXY_AUTH_DIR']
      @plugins_dir = env['CLI_PROXY_PLUGIN_DIR']
      @library = env.fetch('CURSOR_LOCAL_LIBRARY', File.join(PROJECT, 'build', @goos, @goarch, "cursor-local.#{CursorLocal.extension(@goos)}"))
      @dashboard = env['CLI_PROXY_DASHBOARD']
      @dashboard_source = env['CURSOR_LOCAL_DASHBOARD_SOURCE']
      @management_key = env.fetch('CLI_PROXY_MANAGEMENT_KEY_FILE', File.join(root, 'management-key'))
      @client_key = env.fetch('CLI_PROXY_CLIENT_KEY_FILE', File.join(root, 'client-key'))
      @port = 18317
    end

    def parse!(arguments, extra: nil)
      parser = OptionParser.new do |options|
        options.banner = "Usage: ruby #{File.basename($PROGRAM_NAME)} [options]"
        options.on('--config PATH', 'CPA configuration file') { |v| @config = v }
        options.on('--binary PATH', 'CPA executable (or name on PATH)') { |v| @binary = v }
        options.on('--url URL', 'CPA URL; use HTTPS or a loopback SSH tunnel') { |v| @url = v }
        options.on('--auth-dir PATH', 'Override configured credential directory') { |v| @auth_dir = v }
        options.on('--plugins-dir PATH', 'Override configured plugin directory') { |v| @plugins_dir = v }
        options.on('--library PATH', 'Native library built for this machine') { |v| @library = v }
        options.on('--dashboard PATH', 'Installed management.html path') { |v| @dashboard = v }
        options.on('--dashboard-source PATH', 'Built management.html to install') { |v| @dashboard_source = v }
        options.on('--management-key-file PATH') { |v| @management_key = v }
        options.on('--client-key-file PATH') { |v| @client_key = v }
        options.on('--port NUMBER', Integer, 'Isolated host-check port') { |v| @port = v }
        extra&.call(options)
      end
      parser.parse!(arguments)
      raise 'port must be between 1 and 65535' unless (1..65535).cover?(@port)
      @config = File.expand_path(@config)
      self
    end

    def configuration
      raise 'refusing a symlinked config file' if File.symlink?(@config)
      value = YAML.safe_load(File.read(@config), permitted_classes: [], aliases: false)
      raise 'a v8 CPA configuration is required' unless value.is_a?(Hash) && value['config-version'] == 8
      value
    end

    def resolve_paths!(data = configuration)
      @auth_dir ||= data.dig('oauth', 'auth-dir') || File.join(Dir.home, '.cli-proxy-api')
      @plugins_dir ||= data.dig('plugins', 'dir') || File.join(File.dirname(@config), 'plugins')
      @dashboard ||= File.join(File.dirname(@config), 'static', 'management.html')
      @auth_dir, @plugins_dir, @dashboard = [@auth_dir, @plugins_dir, @dashboard].map { |v| File.expand_path(v, File.dirname(@config)) }
      self
    end

    def request(path, key:, timeout: 20, method: :get, body: nil)
      base = URI(@url)
      allowed_http = %w[127.0.0.1 localhost ::1].include?(base.hostname)
      raise 'use HTTPS or a loopback SSH tunnel for authenticated access' unless base.scheme == 'https' || (base.scheme == 'http' && allowed_http)
      raise 'URL must not contain credentials, query or fragment' if base.userinfo || base.query || base.fragment
      target = URI(@url.sub(%r{/+\z}, '') + path)
      raise 'unsupported HTTP method' unless [:get, :post].include?(method)
      request = (method == :post ? Net::HTTP::Post : Net::HTTP::Get).new(target)
      if body
        request['Content-Type'] = 'application/json'
        request.body = body
      end
      request['Authorization'] = "Bearer #{key}" if key
      Net::HTTP.start(target.hostname, target.port, nil, use_ssl: target.scheme == 'https', read_timeout: timeout, open_timeout: 5) { |http| http.request(request) }
    end

    def request_json(path, key:)
      response = request(path, key: key)
      raise "proxy returned HTTP #{response.code}" unless response.code == '200'
      JSON.parse(response.body)
    end
  end

  def self.replace_sections(source, sections)
    updated = source
    sections.each do |name, value|
      pattern = /^#{Regexp.escape(name)}:\s*\n(?:[ \t][^\n]*\n|\n)*/
      block = YAML.dump(name => value).sub(/\A---\n/, '')
      updated = updated.match?(pattern) ? updated.sub(pattern, block) : updated + "\n" + block
    end
    updated
  end

  def self.validate_library(path, goos:, goarch:)
    bytes = File.binread(path, 4096)
    valid = case goos
    when 'linux', 'freebsd'
      machine = bytes.byteslice(18, 2)&.unpack1('v')
      bytes.start_with?("\x7fELF".b) && bytes.getbyte(4) == 2 && bytes.getbyte(5) == 1 && machine == {'amd64' => 62, 'arm64' => 183}[goarch]
    when 'darwin'
      cpu = bytes.byteslice(4, 4)&.unpack1('V')
      bytes.start_with?("\xcf\xfa\xed\xfe".b) && cpu == {'amd64' => 0x1000007, 'arm64' => 0x100000c}[goarch]
    when 'windows'
      offset = bytes.byteslice(60, 4)&.unpack1('V')
      offset && bytes.start_with?('MZ') && bytes.byteslice(offset, 4) == "PE\x00\x00" && bytes.byteslice(offset + 4, 2)&.unpack1('v') == {'amd64' => 0x8664, 'arm64' => 0xaa64}[goarch]
    end
    raise 'library format or architecture does not match the destination' unless valid
  end

  def self.atomic_write(path, content, mode:)
    raise 'refusing a symlinked destination' if File.symlink?(path)
    FileUtils.mkdir_p(File.dirname(path))
    temporary = "#{path}.tmp-#{SecureRandom.hex(8)}"
    begin
      File.open(temporary, File::WRONLY | File::CREAT | File::EXCL, mode) { |file| file.write(content); file.flush; file.fsync }
      File.rename(temporary, path)
    ensure
      FileUtils.rm_f(temporary)
    end
  end

  def self.backup(path)
    return nil unless File.file?(path)
    target = "#{path}.before-cursor-#{Time.now.utc.strftime('%Y%m%dT%H%M%SZ')}-#{SecureRandom.hex(4)}"
    FileUtils.cp(path, target)
    File.chmod(0600, target)
    target
  end
end
