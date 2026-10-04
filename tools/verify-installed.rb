#!/usr/bin/env ruby
# Verify runtime metadata and routes without displaying keys or account tokens.
require 'digest'
require 'json'
require 'net/http'
require 'time'
require_relative 'environment'

security_probe = false
settings = CursorLocal::Environment.new.parse!(ARGV, extra: ->(parser) { parser.on('--security-probe') { security_probe = true } })
raise 'unexpected positional arguments' unless ARGV.empty? || ARGV == ['snapshot']
settings.resolve_paths!
project = CursorLocal::PROJECT
state_path = File.join(project, 'build', 'installation-baseline.json')
request_json = ->(path, key) { settings.request_json(path, key: key) }
client_key = File.read(settings.client_key).strip
management_key = File.read(settings.management_key).strip
models = request_json.call('/v1/models', client_key).fetch('data').map { |model| model.fetch('id') }.sort
credentials = Dir.glob(File.join(settings.auth_dir, '*.json')).to_h { |path| [File.basename(path), Digest::SHA256.file(path).hexdigest] }
if ARGV.first == 'snapshot'
  File.open(state_path, File::WRONLY | File::CREAT | File::TRUNC, 0600) { |file| file.write(JSON.generate({'models' => models, 'credentials' => credentials})) }
  puts "Recorded #{models.length} model routes and #{credentials.length} credential file hashes."
else
  baseline = JSON.parse(File.read(state_path))
  raise 'existing model routes were lost' unless (baseline.fetch('models') - models).empty?
  prior_credentials = baseline.fetch('credentials')
  raise 'credential files were removed or replaced' unless prior_credentials.keys.sort == credentials.keys.sort
  changed = credentials.keys.select { |name| prior_credentials[name] != credentials[name] }
  # This baseline requires exact hashes; re-snapshot after intentional OAuth refresh.
  # No provider-specific refresh exception is portable or account-neutral.
  raise 'credential contents changed; review OAuth refresh before taking a new snapshot' unless changed.empty?
  plugins = request_json.call('/v8/management/plugins', management_key).fetch('plugins')
  plugin = plugins.find { |entry| entry['id'] == 'cursor-local' }
  expected = File.read(File.join(project, 'internal/provider/service.go'))[/Version\s*=\s*"([^"]+)"/, 1]
  raise 'runtime has not loaded the expected version' unless plugin && plugin['effective_enabled'] && plugin.dig('metadata', 'version') == expected
  status = request_json.call('/v0/management/plugins/cursor-local/status', management_key)
  raise 'wrong management provider' unless status['provider'] == 'cursor-local'
  resource = settings.request('/v0/resource/plugins/cursor-local/status', key: nil, timeout: 5)
  raise 'static management resource missing' unless resource.code == '200' && resource.body.include?('Cursor Local')
  # Keep deliberate authentication failures out of ordinary production checks.
  # The isolated host check always verifies this boundary.
  if security_probe
    denied = settings.request('/v0/management/plugins/cursor-local/status', key: nil, timeout: 5)
    raise 'management API lacks authentication' unless ['401', '403'].include?(denied.code)
    puts 'PASS: explicit authentication probe rejected (one expected WARN in the proxy log).'
  end
  raise 'Cursor quota capability missing' unless plugin['supports_quota']
  status.fetch('accounts').each do |account|
    quota = request_json.call('/v8/management/plugins/cursor-local/quota?auth_index=' + account.fetch('auth_index'), management_key)
    local = quota.fetch('summary').select { |metric| metric['key'].start_with?('local_', 'estimated_') }
    raise 'local usage missing' unless local.length == 5
    raise 'verified subscription plan or usage missing' unless quota.dig('subscription', 'plan') && quota.fetch('groups').length >= 2
  end
  puts "PASS: cursor-local #{expected} enabled; #{models.length} routes and #{credentials.length} account files preserved."
  puts "PASS: quota provider registered; authenticated status and static page available; #{status.fetch('accounts').length} Cursor accounts."
end
