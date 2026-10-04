#!/usr/bin/env ruby
# Install a locally built library while preserving other plugins and credentials.
require 'digest'
require 'time'
require_relative 'environment'

settings = CursorLocal::Environment.new.parse!(ARGV)
raise 'unexpected positional arguments' unless ARGV.empty?
before = settings.configuration
settings.resolve_paths!(before)
raise 'build a native library first or pass --library' unless File.file?(settings.library)
CursorLocal.validate_library(settings.library, goos: settings.goos, goarch: settings.goarch)
after = Marshal.load(Marshal.dump(before))
plugins = after['plugins'] ||= {}
plugins['enabled'] = true
plugins['dir'] = settings.plugins_dir
configs = plugins['configs'] ||= {}
configs['cursor-local'] = (configs['cursor-local'] || {}).merge('enabled' => true, 'priority' => 1)
updated = CursorLocal.replace_sections(File.read(settings.config), 'plugins' => plugins)
raise 'config preservation check failed' unless YAML.safe_load(updated, permitted_classes: [], aliases: false) == after
library_dir = File.join(settings.plugins_dir, settings.goos, settings.goarch)
library = File.join(library_dir, "cursor-local.#{CursorLocal.extension(settings.goos)}")
raise 'refusing a symlinked plugin directory' if [settings.plugins_dir, File.dirname(library_dir), library_dir].any? { |p| File.symlink?(p) }
raise 'refusing a symlinked library' if File.symlink?(library)
backup = CursorLocal.backup(settings.config)
CursorLocal.backup(library)
FileUtils.mkdir_p(library_dir, mode: 0700)
[settings.plugins_dir, File.dirname(library_dir), library_dir].each { |path| File.chmod(0700, path) }
CursorLocal.atomic_write(library, File.binread(settings.library), mode: 0700)
CursorLocal.atomic_write(settings.config, updated, mode: 0600)
sha = Digest::SHA256.file(library).hexdigest
CursorLocal.atomic_write(File.join(CursorLocal::PROJECT, 'build', 'installation.md'), <<~DOC, mode: 0600)
  # Installation record

  Installed #{Time.now.utc.iso8601} for #{settings.goos}/#{settings.goarch}.

  - Library: `#{library}`
  - SHA-256: `#{sha}`
  - Config backup: `#{backup}`
  - Source: `#{CursorLocal::PROJECT}`

  Only plugin settings changed. Credentials were not written. Restart CPA
  after installation. The library must match the host OS and architecture.
DOC
puts "Installed cursor-local for #{settings.goos}/#{settings.goarch}. Config backup: #{backup}"
puts "Library SHA-256: #{sha}"
