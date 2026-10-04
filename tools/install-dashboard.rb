#!/usr/bin/env ruby
# Install an explicitly selected management build and preserve other settings.
require 'digest'
require_relative 'environment'

settings = CursorLocal::Environment.new.parse!(ARGV)
raise 'unexpected positional arguments' unless ARGV.empty?
raise 'pass --dashboard-source /path/to/dist/index.html' unless settings.dashboard_source && File.file?(settings.dashboard_source)
before = settings.configuration
settings.resolve_paths!(before)
after = Marshal.load(Marshal.dump(before))
after.fetch('management')['disable-auto-update-panel'] = true
after.fetch('oauth')['excluded-models'] ||= {}
after.fetch('oauth')['model-alias'] ||= {}
updated = CursorLocal.replace_sections(File.read(settings.config), 'management' => after.fetch('management'), 'oauth' => after.fetch('oauth'))
raise 'config preservation check failed' unless YAML.safe_load(updated, permitted_classes: [], aliases: false) == after
raise 'refusing a symlinked dashboard' if File.symlink?(settings.dashboard)
backup = CursorLocal.backup(settings.config)
CursorLocal.backup(settings.dashboard)
CursorLocal.atomic_write(settings.dashboard, File.binread(settings.dashboard_source), mode: 0644)
CursorLocal.atomic_write(settings.config, updated, mode: 0600)
puts "Installed Cursor quota dashboard at #{settings.dashboard}. Config backup: #{backup}"
puts "Dashboard SHA-256: #{Digest::SHA256.file(settings.dashboard).hexdigest}"
