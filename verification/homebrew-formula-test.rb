# Run with brew ruby after copying the formula into a temporary registered tap.
# Exercises the actual Homebrew DSL, install recipe and test block in a private
# prefix. It does not install a keg or dependencies into the caller's Homebrew.
require "tmpdir"
require "digest"
require "formula_assertions"

raise "expected formula path, source directory, spec, archive and Go executable" unless ARGV.length == 5
formula_path = Pathname(ARGV.fetch(0)).realpath
source = Pathname(ARGV.fetch(1)).realpath
spec = ARGV.fetch(2, "head").to_sym
raise "expected head or stable formula spec" unless [:head, :stable].include?(spec)

Dir.mktmpdir("irrigator-homebrew-") do |temporary|
  prefix_path = Pathname(temporary)/"prefix"
  formula = Formulary.factory(formula_path, spec)
  formula.extend(Homebrew::Assertions)
  formula.build = BuildOptions.new(Options.create(spec == :head ? ["--HEAD"] : []), formula.options)
  raise "expected Irrigator formula" unless formula.name == "irrigator"
  raise "HEAD recipe must declare its Go build dependency" if spec == :head && formula.deps.none? { |dep| dep.name == "go" && dep.build? }
  raise "stable archives must not require Go" if spec == :stable && formula.deps.any? { |dep| dep.name == "go" }
  if spec == :stable
    raise "stable recipe checksum differs from candidate" unless formula.stable.checksum.to_s == Digest::SHA256.file(ARGV.fetch(3)).hexdigest
  end
  formula.define_singleton_method(:prefix) { |*_args| prefix_path }
  formula.buildpath = source
  ENV.activate_extensions!(env: "std")
  # brew ruby starts with a system-only PATH. A normal install supplies its Go
  # dependency; this private recipe check uses the caller's existing compiler.
  ENV.prepend_path "PATH", Pathname(ARGV.fetch(4)).dirname if spec == :head
  Dir.chdir(source) { formula.install }
  raise "recipe did not install the executable" unless formula.bin.join("irrigator").executable?
  formula.run_test
  puts "PASS: Homebrew #{spec} recipe installed Irrigator in a private prefix and passed its mock test."
end
