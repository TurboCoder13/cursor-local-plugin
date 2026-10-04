#!/usr/bin/env ruby
# Exercise deployment behavior in temporary directories, never the real install.
require 'minitest/autorun'
require 'open3'
require 'tmpdir'
require_relative 'environment'

class PortabilityTest < Minitest::Test
  def test_platforms_and_native_extensions
    [['linux-gnu', 'x86_64', 'linux', 'amd64', 'so'], ['linux-gnu', 'aarch64', 'linux', 'arm64', 'so'], ['darwin24', 'arm64', 'darwin', 'arm64', 'dylib'], ['mingw32', 'x86_64', 'windows', 'amd64', 'dll'], ['freebsd14', 'amd64', 'freebsd', 'amd64', 'so']].each do |os, cpu, goos, arch, ext|
      assert_equal [goos, arch], CursorLocal.platform(os: os, cpu: cpu)
      assert_equal ext, CursorLocal.extension(goos)
    end
    assert_raises(RuntimeError) { CursorLocal.platform(os: 'unsupported', cpu: 'x86_64') }
  end

  def test_paths_can_be_configured_without_a_homebrew_install
    settings = CursorLocal::Environment.new(env: {'XDG_CONFIG_HOME' => '/custom/config'}, home: '/custom/home', platform: ['linux', 'amd64'])
    settings.parse!(['--config', '/custom/cpa.yaml', '--binary', '/custom/bin/cpa', '--plugins-dir', '/custom/plugins'])
    settings.resolve_paths!({'oauth' => {'auth-dir' => './accounts'}})
    assert_equal '/custom/cpa.yaml', settings.config
    assert_equal '/custom/accounts', settings.auth_dir
    assert_equal '/custom/plugins', settings.plugins_dir
    assert_equal '/custom/bin/cpa', settings.binary
    assert settings.library.end_with?('/linux/amd64/cursor-local.so')
  end

  def test_installer_preserves_credentials_other_plugins_and_all_other_config
    Dir.mktmpdir do |dir|
      config, original = fixture(dir)
      library = native_fixture(dir)
      output, errors, status = run_tool('install.rb', '--config', config, '--library', library)
      assert status.success?, "installer failed: #{errors}"
      assert_includes output, 'Installed cursor-local'
      after = YAML.safe_load(File.read(config))
      assert_equal original.reject { |k, _| k == 'plugins' }, after.reject { |k, _| k == 'plugins' }
      assert_equal original.dig('plugins', 'configs', 'other-provider'), after.dig('plugins', 'configs', 'other-provider')
      assert_equal 'retained', after.dig('plugins', 'configs', 'cursor-local', 'custom-setting')
      assert_equal 'credential-fixture', File.read(File.join(dir, 'accounts', 'one.json'))
      os, arch = CursorLocal.platform
      installed = File.join(dir, 'plugins', os, arch, "cursor-local.#{CursorLocal.extension(os)}")
      assert_equal File.binread(library), File.binread(installed)
      assert_equal 0600, File.stat(config).mode & 0777 unless os == 'windows'
      assert_equal 1, Dir.glob(config + '.before-cursor-*').length
    end
  end

  def test_wrong_architecture_does_not_mutate_config
    Dir.mktmpdir do |dir|
      config, = fixture(dir)
      before = File.binread(config)
      library = File.join(dir, 'invalid-library')
      File.write(library, 'not a library')
      _, errors, status = run_tool('install.rb', '--config', config, '--library', library)
      refute status.success?
      assert_includes errors, 'format or architecture'
      assert_equal before, File.binread(config)
      assert_empty Dir.glob(config + '.before-cursor-*')
    end
  end

  def test_symlinked_config_is_rejected
    Dir.mktmpdir do |dir|
      config, = fixture(dir)
      link = File.join(dir, 'linked.yaml')
      File.symlink(config, link)
      _, errors, status = run_tool('install.rb', '--config', link)
      refute status.success?
      assert_includes errors, 'symlinked config'
    end
  end

  def test_dashboard_installs_at_a_new_destination_and_preserves_config
    Dir.mktmpdir do |dir|
      config, original = fixture(dir)
      source = File.join(dir, 'built.html')
      File.write(source, '<html>fixture dashboard</html>')
      _, errors, status = run_tool('install-dashboard.rb', '--config', config, '--dashboard-source', source)
      assert status.success?, "dashboard installation failed: #{errors}"
      after = YAML.safe_load(File.read(config))
      assert_equal true, after.dig('management', 'disable-auto-update-panel')
      assert_equal original['plugins'], after['plugins']
      assert_equal original['access'], after['access']
      assert_equal original.dig('oauth', 'auth-dir'), after.dig('oauth', 'auth-dir')
      assert_equal File.read(source), File.read(File.join(dir, 'static', 'management.html'))
    end
  end

  def test_fresh_setup_stores_keys_privately_and_refuses_reset
    Dir.mktmpdir do |dir|
      path = File.join(dir, 'deployment', 'config.yaml')
      management = File.join(dir, 'keys', 'management')
      client = File.join(dir, 'keys', 'client')
      args = ['--config', path, '--management-key-file', management, '--client-key-file', client]
      output, errors, status = run_tool('setup.rb', *args)
      assert status.success?, "setup failed: #{errors}"
      data = YAML.safe_load(File.read(path))
      assert_equal 64, File.read(management).strip.length
      assert_equal File.read(management).strip, data.dig('management', 'secret-key')
      assert_equal [File.read(client).strip], data.dig('access', 'api-keys')
      refute_includes output, File.read(management).strip
      assert_equal true, data.dig('observability', 'logs', 'logging-to-file')
      before = File.read(path)
      _, _, second = run_tool('setup.rb', *args)
      refute second.success?
      assert_equal before, File.read(path)
    end
  end

  def test_plain_http_remote_url_is_rejected_before_network_access
    settings = CursorLocal::Environment.new(env: {}, home: '/tmp')
    settings.url = 'http://example.invalid'
    assert_raises(RuntimeError) { settings.request('/v1/models', key: 'fixture') }
    settings.url = 'https://user:password@example.invalid'
    assert_raises(RuntimeError) { settings.request('/v1/models', key: 'fixture') }
  end

  private

  def fixture(dir)
    accounts = File.join(dir, 'accounts')
    FileUtils.mkdir_p(accounts)
    File.write(File.join(accounts, 'one.json'), 'credential-fixture')
    data = {'config-version' => 8, 'server' => {'host' => '127.0.0.1', 'port' => 8317}, 'management' => {'secret-key' => 'fixture'}, 'access' => {'api-keys' => ['fixture']}, 'oauth' => {'auth-dir' => accounts}, 'plugins' => {'dir' => File.join(dir, 'plugins'), 'configs' => {'other-provider' => {'enabled' => true}, 'cursor-local' => {'custom-setting' => 'retained'}}}}
    path = File.join(dir, 'config.yaml')
    File.write(path, YAML.dump(data))
    [path, data]
  end

  def native_fixture(dir)
    os, arch = CursorLocal.platform
    bytes = "\x00".b * 128
    case os
    when 'darwin'
      bytes[0, 4] = "\xcf\xfa\xed\xfe".b
      bytes[4, 4] = [arch == 'amd64' ? 0x1000007 : 0x100000c].pack('V')
    when 'windows'
      bytes[0, 2] = 'MZ'
      bytes[60, 4] = [64].pack('V')
      bytes[64, 4] = "PE\x00\x00"
      bytes[68, 2] = [arch == 'amd64' ? 0x8664 : 0xaa64].pack('v')
    else
      bytes[0, 6] = "\x7fELF\x02\x01".b
      bytes[18, 2] = [arch == 'amd64' ? 62 : 183].pack('v')
    end
    path = File.join(dir, 'library')
    File.binwrite(path, bytes)
    path
  end

  def run_tool(name, *args)
    # nosemgrep: ruby.lang.security.dangerous-exec.dangerous-exec -- fixed interpreter, test-owned script and separate argv; no shell expansion.
    Open3.capture3('ruby', File.join(__dir__, name), *args)
  end
end
