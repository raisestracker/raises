# frozen_string_literal: true

require_relative "lib/raises/version"

Gem::Specification.new do |spec|
  spec.name = "raises"
  spec.version = Raises::VERSION
  spec.authors = ["Clayton Lengel-Zigich"]
  spec.summary = "Rails.error subscriber that reports exceptions to a raises inbox"
  spec.homepage = "https://raises.dev"
  spec.license = "MIT"
  spec.description = "A Rails.error subscriber for the agent-operated Raises exception inbox."
  spec.files = Dir["lib/**/*.rb", "README.md", "CHANGELOG.md", "LICENSE.txt"]
  spec.require_paths = ["lib"]
  spec.required_ruby_version = ">= 3.2"
  spec.metadata = {
    "homepage_uri" => "https://raises.dev",
    "source_code_uri" => "https://github.com/raisestracker/raises",
    "changelog_uri" => "https://github.com/raisestracker/raises/blob/master/gem/CHANGELOG.md",
    "bug_tracker_uri" => "https://github.com/raisestracker/raises/issues",
    "rubygems_mfa_required" => "true"
  }
  spec.add_dependency "railties", ">= 7.1"
end
