# frozen_string_literal: true

require "minitest/autorun"
require "raises"

class DeliveryTest < Minitest::Test
  FakeSpool = Struct.new(:notices) do
    def enqueue(notice)
      notices << notice
      :queued
    end
  end

  def setup
    @notices = []
    @warnings = []
    @spool = FakeSpool.new(@notices)
  end

  def test_transient_http_failure_is_spooled
    response = Net::HTTPServiceUnavailable.new("1.1", "503", "Unavailable")
    delivery = build_delivery { response }

    delivery.call({ "error" => { "class" => "RuntimeError" } }, path: "v1/notices")

    assert_equal 1, @notices.length
    assert_equal "v1/notices", @notices.first.fetch("path")
    assert_includes @warnings, "raises queued notice after HTTP 503"
  end

  def test_permanent_http_failure_is_not_spooled
    response = Net::HTTPUnprocessableEntity.new("1.1", "422", "Invalid")
    delivery = build_delivery { response }

    delivery.call({ "error" => { "class" => "RuntimeError" } }, path: "v1/notices")

    assert_empty @notices
    assert_includes @warnings, "raises rejected notice: HTTP 422"
  end

  def test_network_exception_is_spooled
    delivery = build_delivery { raise Net::ReadTimeout }

    delivery.call({ "message" => "deploy finished" }, path: "v1/events")

    assert_equal 1, @notices.length
    assert_equal "v1/events", @notices.first.fetch("path")
    assert(@warnings.any? { |warning| warning.include?("Net::ReadTimeout") })
  end

  private

  def build_delivery(&post)
    Raises::Delivery.new(
      post: ->(_path, _payload) { post.call },
      spool: @spool,
      warn: ->(message) { @warnings << message }
    )
  end
end
