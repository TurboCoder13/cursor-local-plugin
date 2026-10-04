#!/usr/bin/env ruby
# Load the compiled library in the installed CPA binary with an isolated auth dir.
require 'fileutils'
require 'json'
require 'net/http'
require 'tmpdir'
require 'yaml'
require_relative 'environment'

preview = false
settings = CursorLocal::Environment.new.parse!(ARGV, extra: ->(parser) { parser.on('--preview') { preview = true } })
raise 'unexpected positional arguments' unless ARGV.empty?
project = CursorLocal::PROJECT
raise 'native library missing' unless File.file?(settings.library)
Dir.mktmpdir('cursor-local-host-check-') do |scratch|
  plugins = File.join(scratch, 'plugins', settings.goos, settings.goarch)
  FileUtils.mkdir_p(plugins)
  FileUtils.cp(settings.library, plugins)
  config = {
    'config-version' => 8,
    'server' => {'host' => '127.0.0.1', 'port' => settings.port, 'discovery' => {'enabled' => false}},
    'management' => {'secret-key' => 'isolated-host-test', 'allow-remote' => false, 'disable-control-panel' => true},
    'access' => {'api-keys' => ['isolated-client-test']},
    'oauth' => {'auth-dir' => File.join(scratch, 'auth')},
    'plugins' => {'enabled' => true, 'dir' => File.join(scratch, 'plugins'), 'configs' => {'cursor-local' => {'enabled' => true}}}
  }
  path = File.join(scratch, 'config.yaml')
  File.write(path, YAML.dump(config))
  log = File.join(scratch, 'runtime.log')
  pid = Process.spawn(settings.binary, '-config', path, '-local-model', '-standalone', out: log, err: log, chdir: scratch)
  begin
    response = nil
    50.times do
      begin
        request = Net::HTTP::Get.new('/v8/management/plugins')
        request['Authorization'] = 'Bearer isolated-host-test'
        response = Net::HTTP.start('127.0.0.1', settings.port, read_timeout: 2) { |http| http.request(request) }
        break if response.code == '200'
      rescue Errno::ECONNREFUSED
        sleep 0.1
      end
    end
    raise 'isolated host failed to start' unless response&.code == '200'
    data = JSON.parse(response.body)
    plugin = data.fetch('plugins').find { |entry| entry['id'] == 'cursor-local' }
    raise "plugin not active; check isolated runtime log" unless plugin && plugin['effective_enabled'] && plugin.dig('metadata', 'version') == File.read(File.join(project, 'internal/provider/service.go'))[/Version\s*=\s*"([^"]+)"/, 1]
    raise 'quota capability did not register' unless plugin['supports_quota']
    # Public resource must be static; management account data must require the key.
    resource = Net::HTTP.start('127.0.0.1', settings.port, read_timeout: 2) { |http| http.get('/v0/resource/plugins/cursor-local/status') }
    raise 'management resource missing' unless resource.code == '200' && resource.body.include?('Cursor Local') && resource['Content-Security-Policy'].include?("connect-src 'self'")
    status_path = '/v0/management/plugins/cursor-local/status'
    denied = Net::HTTP.start('127.0.0.1', settings.port, read_timeout: 2) { |http| http.get(status_path) }
    raise 'account status was accessible without authentication' unless ['401', '403'].include?(denied.code)
    status_request = Net::HTTP::Get.new(status_path)
    status_request['Authorization'] = 'Bearer isolated-host-test'
    status = Net::HTTP.start('127.0.0.1', settings.port, read_timeout: 2) { |http| http.request(status_request) }
    raise 'authenticated plugin status failed' unless status.code == '200' && JSON.parse(status.body).fetch('accounts') == []
    request = Net::HTTP::Get.new('/v8/management/oauth/auth-url?provider=cursor-local')
    request['Authorization'] = 'Bearer isolated-host-test'
    response = Net::HTTP.start('127.0.0.1', settings.port, read_timeout: 2) { |http| http.request(request) }
    login = JSON.parse(response.body)
    raise 'login registration failed' unless response.code == '200' && login['url'].start_with?('https://cursor.com/loginDeepControl?') && login['state']
    puts "PASS: native #{settings.goos}/#{settings.goarch} library loads; OAuth, quota and authenticated management routes register."
    puts 'No Cursor account was used; no inference request was made.'
    if preview
      puts "Preview: http://127.0.0.1:#{settings.port}/v0/resource/plugins/cursor-local/status"
      STDOUT.flush
      loop { sleep 1 }
    end
  ensure
    Process.kill('TERM', pid) rescue Errno::ESRCH
    Process.wait(pid)
    FileUtils.cp(log, File.join(project, 'build', 'host-check.log'))
  end
end
