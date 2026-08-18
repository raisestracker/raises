# frozen_string_literal: true

require "fileutils"
require "json"
require "securerandom"

module Raises
  class SpoolStorage
    MAX_NOTICES = 1_000
    MAX_BYTES = 100 * 1024 * 1024

    def initialize(directory, warn:, now:)
      @directory = File.expand_path(directory)
      @warn = warn
      @now = now
      prepare
    end

    def store(item)
      prepare
      raw = JSON.generate(
        "version" => 2,
        "attempts" => 0,
        "next_attempt_at" => @now.call.to_f,
        "path" => item.fetch("path"),
        "payload" => item.fetch("payload")
      )
      with_directory_lock do
        return :full unless capacity_for?(raw)

        atomic_write(File.join(@directory, filename), raw)
      end
      :stored
    end

    def each_due(limit:)
      processed = 0
      files.each do |path|
        break if processed >= limit

        File.open(path, File::RDWR) do |file|
          next unless file.flock(File::LOCK_EX | File::LOCK_NB)

          envelope = parse(file.read, path)
          next unless envelope
          next if envelope.fetch("next_attempt_at", 0).to_f > @now.call.to_f

          processed += 1
          yield path, envelope
        end
      rescue Errno::ENOENT
        next
      end
      processed
    end

    def delete(path)
      File.delete(path)
    rescue Errno::ENOENT
      nil
    end

    def rewrite(path, envelope)
      atomic_write(path, JSON.generate(envelope))
    end

    private

    def prepare
      FileUtils.mkdir_p(@directory, mode: 0o700)
      File.chmod(0o700, @directory)
    end

    def files
      Dir.glob(File.join(@directory, "*.json"))
    end

    def capacity_for?(raw)
      paths = files
      return false if paths.length >= MAX_NOTICES

      paths.sum { |path| file_size(path) } + raw.bytesize <= MAX_BYTES
    end

    def file_size(path)
      File.size(path)
    rescue Errno::ENOENT
      0
    end

    def filename
      format("%<time>020d-%<random>s.json", time: (@now.call.to_f * 1_000_000).to_i, random: SecureRandom.hex(8))
    end

    def with_directory_lock
      File.open(File.join(@directory, ".lock"), File::WRONLY | File::CREAT, 0o600) do |lock|
        lock.flock(File::LOCK_EX)
        yield
      end
    end

    def atomic_write(path, raw)
      temporary = "#{path}.tmp-#{Process.pid}-#{SecureRandom.hex(4)}"
      File.open(temporary, File::WRONLY | File::CREAT | File::EXCL, 0o600) do |file|
        file.write(raw)
        file.flush
        file.fsync
      end
      File.rename(temporary, path)
    ensure
      File.delete(temporary) if defined?(temporary) && File.exist?(temporary)
    end

    def parse(raw, path)
      envelope = JSON.parse(raw)
      return envelope if envelope.is_a?(Hash) && envelope["path"].is_a?(String) && envelope["payload"].is_a?(Hash)

      if envelope.is_a?(Hash) && envelope["notice"].is_a?(Hash)
        envelope["version"] = 2
        envelope["path"] = "v1/notices"
        envelope["payload"] = envelope.delete("notice")
        return envelope
      end

      raise JSON::ParserError, "invalid envelope"
    rescue JSON::ParserError, TypeError => e
      quarantine(path)
      @warn.call("raises quarantined corrupt spool entry: #{e.message}")
      nil
    end

    def quarantine(path)
      target = path.sub(/\.json\z/, ".corrupt")
      File.rename(path, target)
      File.chmod(0o600, target)
    rescue Errno::ENOENT
      nil
    end
  end
end
