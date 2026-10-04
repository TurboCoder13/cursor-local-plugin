#!/usr/bin/env ruby
# Fetch a pinned CPA runtime, verifying its published checksum before extraction.
require 'digest'
require 'open3'
require 'rubygems/package'
require 'zlib'
require_relative 'environment'

settings = CursorLocal::Environment.new.parse!(ARGV)
raise 'unexpected positional arguments' unless ARGV.empty?
metadata = JSON.parse(File.read(File.join(CursorLocal::PROJECT, 'deploy', 'runtime.json')))
version = metadata.fetch('version')
repository = metadata.fetch('repository')
raise 'invalid runtime release metadata' unless version.match?(/\A[0-9]+\.[0-9]+\.[0-9]+\z/) && repository.match?(%r{\A[A-Za-z0-9._-]+/[A-Za-z0-9._-]+\z})
arch = settings.goarch == 'arm64' ? 'aarch64' : settings.goarch
suffix = settings.goos == 'windows' ? 'zip' : 'tar.gz'
archive_name = "CLIProxyAPI_#{version}_#{settings.goos}_#{arch}.#{suffix}"
root = File.join(CursorLocal::PROJECT, 'build', 'runtime')
FileUtils.mkdir_p(root)
_, _, status = Open3.capture3('gh', 'release', 'download', "v#{version}", '--repo', repository, '--pattern', archive_name, '--pattern', 'checksums.txt', '--dir', root, '--clobber')
raise 'runtime download failed; install gh or download the pinned plugin-capable release manually' unless status.success?
archive = File.join(root, archive_name)
expected = File.readlines(File.join(root, 'checksums.txt')).find { |line| line.split.last&.delete_prefix('*') == archive_name }&.split&.first
raise 'published runtime checksum mismatch' unless expected && Digest::SHA256.file(archive).hexdigest == expected
name = settings.goos == 'windows' ? 'cliproxyapi.exe' : 'cliproxyapi'
names = %w[cliproxyapi cli-proxy-api CLIProxyAPI].map { |base| settings.goos == 'windows' ? base + '.exe' : base }
bytes = nil
if suffix == 'zip'
  names.each do |entry|
    candidate, _, extracted = Open3.capture3('unzip', '-p', archive, entry)
    next unless extracted.success? && !candidate.empty?
    raise 'runtime archive contains multiple executables' if bytes
    bytes = candidate
  end
else
  Zlib::GzipReader.open(archive) do |compressed|
    Gem::Package::TarReader.new(compressed) do |tar|
      tar.each do |entry|
        next unless names.include?(entry.full_name) && entry.file?
        raise 'runtime archive contains multiple executables' if bytes
        raise 'runtime artifact is unexpectedly large' if entry.header.size > 256 * 1024 * 1024
        bytes = entry.read
      end
    end
  end
end
raise 'runtime executable missing in archive' unless bytes && !bytes.empty?
target = File.join(root, name)
CursorLocal.atomic_write(target, bytes, mode: 0700)
CursorLocal.validate_library(target, goos: settings.goos, goarch: settings.goarch)
puts "Verified and extracted CPA #{version} for #{settings.goos}/#{settings.goarch}: #{target}"
