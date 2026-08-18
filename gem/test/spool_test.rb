# frozen_string_literal: true

require "minitest/autorun"
require "tmpdir"
require "raises"

class SpoolTest < Minitest::Test
  Response = Struct.new(:code, :headers) do
    def [](name) = headers&.fetch(name, nil)
  end

  def setup
    @directory = Dir.mktmpdir("raises-spool")
    @time = Time.at(1_700_000_000)
    @warnings = []
    @response = Response.new("201", {})
    @deliveries = 0
    @spool = build_spool
  end

  def teardown
    @spool.stop
    FileUtils.remove_entry(@directory)
  end

  def test_enqueue_is_private_bounded_and_does_not_store_ingestion_token
    ENV["RAISES_TOKEN"] = "secret-ingestion-token"

    assert @spool.enqueue(item)

    assert_equal 1, notice_files.length
    path = notice_files.first
    refute_includes File.read(path), ENV.fetch("RAISES_TOKEN")
    assert_equal 0o600, File.stat(path).mode & 0o777
    assert_equal 0o700, File.stat(@directory).mode & 0o777
  ensure
    ENV.delete("RAISES_TOKEN")
  end

  def test_successful_delivery_removes_notice
    @spool.enqueue(item)

    assert_equal 1, @spool.drain_once
    assert_empty notice_files
    assert_equal 1, @deliveries
  end

  def test_retryable_response_reschedules_then_succeeds
    @response = Response.new("503", {})
    @spool.enqueue(item)

    @spool.drain_once
    assert_equal 1, notice_files.length
    envelope = JSON.parse(File.read(notice_files.first))
    assert_equal 1, envelope.fetch("attempts")

    @time += 10
    @response = Response.new("201", {})
    @spool.drain_once
    assert_empty notice_files
  end

  def test_permanent_rejection_is_not_retained
    @response = Response.new("422", {})
    @spool.enqueue(item)

    @spool.drain_once
    assert_empty notice_files
    assert(@warnings.any? { |warning| warning.include?("HTTP 422") })
  end

  def test_corrupt_entry_is_quarantined
    path = File.join(@directory, "broken.json")
    File.write(path, "not-json")
    File.chmod(0o600, path)

    @spool.drain_once

    assert File.exist?(File.join(@directory, "broken.corrupt"))
    assert(@warnings.any? { |warning| warning.include?("quarantined") })
  end

  def test_drains_version_one_notice_envelope
    path = File.join(@directory, "legacy.json")
    envelope = {
      "version" => 1,
      "attempts" => 0,
      "next_attempt_at" => @time.to_f,
      "notice" => { "error" => { "class" => "RuntimeError" } }
    }
    File.write(path, JSON.generate(envelope))
    File.chmod(0o600, path)

    assert_equal 1, @spool.drain_once
    assert_empty notice_files
    assert_equal 1, @deliveries
  end

  def test_full_spool_rejects_newest_notice
    Raises::SpoolStorage::MAX_NOTICES.times do |index|
      File.write(File.join(@directory, format("%<index>04d.json", index: index)), "")
    end

    refute @spool.enqueue(item)
    assert(@warnings.any? { |warning| warning.include?("spool is full") })
  end

  def test_two_drainers_deliver_a_file_once
    @spool.enqueue(item)
    second = build_spool

    [Thread.new { @spool.drain_once }, Thread.new { second.drain_once }].each(&:join)

    assert_equal 1, @deliveries
    assert_empty notice_files
  ensure
    second&.stop
  end

  private

  def item
    { "path" => "v1/notices", "payload" => { "error" => { "class" => "RuntimeError" } } }
  end

  def build_spool
    Raises::Spool.new(
      @directory,
      deliver: lambda { |_path, _payload|
        @deliveries += 1
        sleep(0.01)
        @response
      },
      warn: ->(message) { @warnings << message },
      now: -> { @time },
      random: Random.new(1),
      start_on_enqueue: false
    )
  end

  def notice_files
    Dir.glob(File.join(@directory, "*.json"))
  end
end
