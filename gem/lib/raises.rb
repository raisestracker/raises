# frozen_string_literal: true

require "raises/version"
require "raises/spool_storage"
require "raises/spool"
require "raises/delivery"
require "raises/subscriber"
require "raises/railtie" if defined?(Rails::Railtie)

module Raises
  class << self
    def notify(message, level: :info, context: {}, source: nil)
      subscriber.notify(message, level: level, context: context, source: source)
    end

    def subscriber
      @subscriber ||= Subscriber.new.start
    end
  end
end
