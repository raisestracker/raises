# frozen_string_literal: true

require "net/http"

module Raises
  class Delivery
    def initialize(post:, spool:, warn:)
      @post = post
      @spool = spool
      @warn = warn
    end

    def call(payload, path: "v1/notices")
      item = { "path" => path, "payload" => payload }
      response = @post.call(path, payload)
      return true unless response.respond_to?(:code)
      return true if response.is_a?(Net::HTTPSuccess) || response.code.to_i.between?(200, 299)

      response_accepted?(item, response.code.to_i)
    rescue StandardError => e
      exception_accepted?(item || { "path" => path, "payload" => payload }, e)
    end

    private

    def response_accepted?(item, status)
      if retryable_status?(status) && @spool&.enqueue(item)
        @warn.call("raises queued notice after HTTP #{status}")
        true
      else
        @warn.call("raises rejected notice: HTTP #{status}")
        false
      end
    end

    def exception_accepted?(item, error)
      if @spool&.enqueue(item)
        @warn.call("raises queued notice after #{error.class}")
        true
      else
        @warn.call("raises subscriber failed: #{error.class}: #{error.message}")
        false
      end
    end

    def retryable_status?(status)
      status == 408 || status == 429 || status >= 500
    end
  end
end
