# frozen_string_literal: true

require "json"
require "net/http"
require "uri"

module Raises
  # Rails.error integration and explicit application notices share one delivery path.
  # rubocop:disable Metrics/ClassLength
  class Subscriber
    def initialize
      @spool = build_spool
      @delivery = Delivery.new(post: method(:post), spool: @spool, warn: ->(message) { warn(message) })
    end

    def start = tap { @spool&.start }

    def report(error, handled:, severity:, context:, source: nil)
      return unless report?

      start

      notice = {
        env: env_name,
        revision: revision,
        handled: handled,
        error: {
          class: error.class.name,
          message: error.message.to_s,
          backtrace: Array(error.backtrace)
        },
        context: json_safe(context_without_request(context).merge(severity: severity.to_s, source: source)),
        request: request_from(context)
      }

      @delivery.call(notice, path: "v1/notices")
    rescue StandardError => e
      warn("raises subscriber failed: #{e.class}: #{e.message}")
      false
    end

    def notify(message, level: :info, context: {}, source: nil)
      message = message.to_s.strip
      level = level.to_s
      validate_event!(message, level, context, source)
      return false unless report?

      start
      event = {
        env: env_name,
        revision: revision,
        level: level,
        message: message,
        source: source,
        context: json_safe(context)
      }
      @delivery.call(event, path: "v1/events")
    rescue ArgumentError
      raise
    rescue StandardError => e
      warn("raises notify failed: #{e.class}: #{e.message}")
      false
    end

    private

    def validate_event!(message, level, context, source)
      raise ArgumentError, "message is required" if message.empty?
      raise ArgumentError, "message is too long" if message.length > 2_000
      raise ArgumentError, "level must be info, warning, or error" unless %w[info warning error].include?(level)
      raise ArgumentError, "source is too long" if source.to_s.length > 120
      raise ArgumentError, "context must be a hash" unless context.respond_to?(:each_pair)
    end

    def build_spool
      directory = ENV["RAISES_SPOOL_DIR"].to_s
      return if directory.empty?

      Spool.new(directory, deliver: method(:post), warn: ->(message) { warn(message) })
    rescue StandardError => e
      warn("raises spool unavailable: #{e.class}: #{e.message}")
      nil
    end

    def report?
      return false if url.to_s.empty? || token.to_s.empty?
      return true if ENV["RAISES_REPORT"] == "1"

      env_name == "production"
    end

    def url
      ENV.fetch("RAISES_URL", "https://raises.dev")
    end

    def token
      ENV["RAISES_TOKEN"].to_s
    end

    def env_name
      ENV["RAISES_ENV"].to_s.then { |value| value.empty? ? nil : value } ||
        (defined?(Rails) ? Rails.env.to_s : "production")
    end

    def revision
      first_present(ENV.fetch("RAISES_REVISION", nil), ENV.fetch("KAMAL_VERSION", nil), git_revision)
    end

    def first_present(*values)
      values.find { |value| !value.to_s.empty? }
    end

    def git_revision
      return unless defined?(Rails)

      path = Rails.root.join("REVISION")
      path.read.strip if path.exist?
    rescue StandardError
      nil
    end

    def request_from(context)
      req = context[:request] || context["request"]
      return unless req.respond_to?(:method) && req.respond_to?(:original_url)

      json_safe(
        method: req.method,
        path: filtered_path(req),
        params: req.respond_to?(:filtered_parameters) ? req.filtered_parameters : nil
      )
    end

    def context_without_request(context)
      return {} unless context.respond_to?(:each)

      context.each_with_object({}) do |(key, value), clean|
        clean[key] = value unless key.to_s == "request"
      end
    end

    def filtered_path(request)
      return request.filtered_path if request.respond_to?(:filtered_path)

      uri = URI(request.original_url)
      uri.path.to_s.empty? ? "/" : uri.path
    rescue URI::InvalidURIError
      "/"
    end

    def json_safe(value)
      JSON.parse(JSON.generate(as_jsonable(value)))
    rescue StandardError
      { "unserializable" => value.class.name }
    end

    def as_jsonable(value)
      case value
      when Hash
        value.each_with_object({}) { |(k, v), acc| acc[k.to_s] = as_jsonable(v) }
      when Array
        value.map { |item| as_jsonable(item) }
      when Numeric, TrueClass, FalseClass, NilClass
        value
      else
        value.to_s
      end
    end

    def post(path, payload)
      uri = URI.join(url.end_with?("/") ? url : "#{url}/", path)
      http = Net::HTTP.new(uri.host, uri.port)
      http.use_ssl = uri.scheme == "https"
      http.open_timeout = timeout("RAISES_OPEN_TIMEOUT", 1)
      http.read_timeout = timeout("RAISES_READ_TIMEOUT", 2)
      request = Net::HTTP::Post.new(uri.request_uri)
      request["Authorization"] = "Bearer #{token}"
      request["Content-Type"] = "application/json"
      request["User-Agent"] = "raises-ruby/#{Raises::VERSION}"
      request.body = JSON.generate(payload)
      http.request(request)
    end

    def timeout(name, fallback)
      value = Float(ENV.fetch(name, fallback.to_s))
      value.positive? && value <= 30 ? value : fallback
    rescue ArgumentError
      fallback
    end
  end
  # rubocop:enable Metrics/ClassLength
end
