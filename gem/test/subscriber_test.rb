# frozen_string_literal: true

require "minitest/autorun"
require "raises"

class SubscriberTest < Minitest::Test
  class Error < StandardError
  end
  Request = Struct.new(:http_method, :original_url, :filtered_parameters) do
    def method = http_method
    def filtered_path = "/users?token=[FILTERED]"
  end

  class CapturingSubscriber < Raises::Subscriber
    attr_reader :path, :payload

    private

    def post(path, payload)
      @path = path
      @payload = payload
    end
  end

  def setup
    @original = ENV.to_h.slice("RAISES_TOKEN", "RAISES_REPORT", "RAISES_ENV", "RAISES_REVISION", "RAISES_SPOOL_DIR")
    ENV["RAISES_TOKEN"] = "test-token"
    ENV["RAISES_REPORT"] = "1"
    ENV["RAISES_ENV"] = "test"
    ENV["RAISES_REVISION"] = "abc123"
  end

  def teardown
    %w[RAISES_TOKEN RAISES_REPORT RAISES_ENV RAISES_REVISION RAISES_SPOOL_DIR].each { |key| ENV.delete(key) }
    @original.each { |key, value| ENV[key] = value }
  end

  def test_reports_filtered_request_without_serializing_request_object
    error = Error.new("boom")
    error.set_backtrace(["app/models/user.rb:4"])
    request = Request.new("POST", "https://example.test/users?token=secret", { token: "[FILTERED]" })
    subscriber = CapturingSubscriber.new

    subscriber.report(error, handled: false, severity: :error, context: { request: request, account_id: 7 })

    assert_equal "v1/notices", subscriber.path
    assert_equal "abc123", subscriber.payload[:revision]
    assert_equal "/users?token=[FILTERED]", subscriber.payload[:request]["path"]
    assert_equal({ "token" => "[FILTERED]" }, subscriber.payload[:request]["params"])
    refute subscriber.payload[:context].key?("request")
    assert_equal 7, subscriber.payload[:context]["account_id"]
  end

  def test_does_not_report_without_token
    ENV.delete("RAISES_TOKEN")
    subscriber = CapturingSubscriber.new
    subscriber.report(Error.new("boom"), handled: false, severity: :error, context: {})
    assert_nil subscriber.payload
  end

  def test_sends_informational_event
    subscriber = CapturingSubscriber.new

    assert subscriber.notify("Deploy finished", level: :info, context: { version: "abc123" }, source: "deploy")

    assert_equal "v1/events", subscriber.path
    assert_equal "Deploy finished", subscriber.payload[:message]
    assert_equal "info", subscriber.payload[:level]
    assert_equal "deploy", subscriber.payload[:source]
    assert_equal({ "version" => "abc123" }, subscriber.payload[:context])
  end

  def test_rejects_invalid_event_level
    subscriber = CapturingSubscriber.new

    error = assert_raises(ArgumentError) { subscriber.notify("Deploy finished", level: :debug) }

    assert_equal "level must be info, warning, or error", error.message
  end

  def test_rejects_invalid_event_context
    subscriber = CapturingSubscriber.new

    error = assert_raises(ArgumentError) { subscriber.notify("Deploy finished", context: "not a hash") }

    assert_equal "context must be a hash", error.message
  end
end
