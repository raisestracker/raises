# frozen_string_literal: true

module Raises
  class Railtie < Rails::Railtie
    initializer "raises.subscribe" do
      Rails.error.subscribe(Raises.subscriber)
    end
  end
end
