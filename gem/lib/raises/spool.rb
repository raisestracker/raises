# frozen_string_literal: true

require_relative "spool_storage"

module Raises
  class Spool
    RETRYABLE_STATUS = [408, 429].freeze

    def initialize(directory, deliver:, warn:, **options)
      @deliver = deliver
      @warn = warn
      @now = options.fetch(:now, -> { Time.now })
      @random = options.fetch(:random, Random.new)
      @start_on_enqueue = options.fetch(:start_on_enqueue, true)
      @storage = SpoolStorage.new(directory, warn: warn, now: @now)
      reset_process_state
    end

    def start
      reset_process_state if @pid != Process.pid
      @mutex.synchronize do
        return if @thread&.alive?

        @thread = Thread.new { run }
        @thread.name = "raises-spool" if @thread.respond_to?(:name=)
        @thread.report_on_exception = false
      end
    end

    def stop
      return if @pid != Process.pid

      thread = @mutex.synchronize do
        @stopping = true
        @condition.broadcast
        @thread
      end
      thread&.join(1)
    end

    def enqueue(item)
      accepted = @storage.store(item) == :stored
      if accepted
        start if @start_on_enqueue
        wake if @start_on_enqueue
      else
        @warn.call("raises spool is full; notice was not queued")
      end
      accepted
    rescue StandardError => e
      @warn.call("raises could not queue notice: #{e.class}: #{e.message}")
      false
    end

    def drain_once(limit: 20)
      @storage.each_due(limit: limit) do |path, envelope|
        deliver(path, envelope)
      end
    end

    private

    def reset_process_state
      @pid = Process.pid
      @mutex = Mutex.new
      @condition = ConditionVariable.new
      @thread = nil
      @stopping = false
    end

    def deliver(path, envelope)
      response = @deliver.call(envelope.fetch("path"), envelope.fetch("payload"))
      status = response_code(response)
      if status.between?(200, 299)
        @storage.delete(path)
      elsif retryable_status?(status)
        reschedule(path, envelope, retry_after(response))
      else
        @storage.delete(path)
        @warn.call("raises rejected queued notice: HTTP #{status}")
      end
    rescue StandardError => e
      reschedule(path, envelope)
      @warn.call("raises queued notice retry failed: #{e.class}: #{e.message}")
    end

    def response_code(response)
      Integer(response.respond_to?(:code) ? response.code : response)
    rescue ArgumentError, TypeError
      500
    end

    def retryable_status?(status)
      RETRYABLE_STATUS.include?(status) || status >= 500
    end

    def retry_after(response)
      return unless response.respond_to?(:[])

      seconds = Integer(response["Retry-After"], exception: false)
      seconds&.clamp(1, 3_600)
    end

    def reschedule(path, envelope, requested_delay = nil)
      attempts = envelope.fetch("attempts", 0).to_i + 1
      base = [2**[attempts, 8].min, 300].min
      delay = requested_delay || (base + (@random.rand * base * 0.2))
      envelope["attempts"] = attempts
      envelope["next_attempt_at"] = @now.call.to_f + delay
      @storage.rewrite(path, envelope)
    end

    def run
      loop do
        drain_once
        @mutex.synchronize do
          break if @stopping

          @condition.wait(@mutex, 5)
          break if @stopping
        end
      end
    rescue StandardError => e
      @warn.call("raises spool worker stopped: #{e.class}: #{e.message}")
    end

    def wake
      return if @pid != Process.pid

      @mutex.synchronize { @condition.broadcast }
    end
  end
end
