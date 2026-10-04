#!/usr/bin/env ruby
# Create a fresh portable configuration; never overwrite an existing install.
require_relative 'environment'

settings = CursorLocal::Environment.new.parse!(ARGV)
raise 'unexpected positional arguments' unless ARGV.empty?
raise 'configuration already exists; setup will not reset it' if File.exist?(settings.config) || File.symlink?(settings.config)
root = File.dirname(settings.config)
settings.auth_dir ||= File.join(root, 'auth')
settings.plugins_dir ||= File.join(root, 'plugins')
settings.resolve_paths!({'oauth' => {}, 'plugins' => {}})
raise 'key files already exist; choose a new deployment directory' if [settings.management_key, settings.client_key].any? { |path| File.exist?(path) || File.symlink?(path) }
management_key, client_key = [SecureRandom.hex(32), SecureRandom.hex(32)]
data = {
  'config-version' => 8,
  'server' => {'host' => '127.0.0.1', 'port' => 8317, 'discovery' => {'enabled' => false}},
  'management' => {'secret-key' => management_key, 'allow-remote' => false, 'disable-auto-update-panel' => true},
  'access' => {'api-keys' => [client_key]},
  'oauth' => {'auth-dir' => settings.auth_dir, 'excluded-models' => {}, 'model-alias' => {}},
  'plugins' => {'enabled' => true, 'dir' => settings.plugins_dir, 'configs' => {'cursor-local' => {'enabled' => true}}},
  'routing' => {'strategy' => 'round-robin'},
  'observability' => {'logs' => {'logging-to-file' => true, 'logs-max-total-size-mb' => 100}, 'usage' => {'usage-statistics-enabled' => true}}
}
[root, settings.auth_dir, settings.plugins_dir].each do |path|
  raise 'refusing a symlinked deployment directory' if File.symlink?(path)
  FileUtils.mkdir_p(path, mode: 0700)
  File.chmod(0700, path)
end
CursorLocal.atomic_write(settings.management_key, management_key + "\n", mode: 0600)
CursorLocal.atomic_write(settings.client_key, client_key + "\n", mode: 0600)
CursorLocal.atomic_write(settings.config, YAML.dump(data), mode: 0600)
puts "Created #{settings.config}; keys are stored in files and were not printed."
puts 'Install the plugin and dashboard before starting CPA. Provider accounts must be signed in or transferred separately.'
