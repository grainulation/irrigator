# Run with brew ruby after copying the formula into a temporary registered tap.
# Exercises the actual Homebrew DSL, install recipe and test block in a private
# prefix. It does not install a keg or formula dependencies. Homebrew may
# initialize its own pinned formula-test gems, as it does for `brew test`.
require "tmpdir"
require "digest"
# Match `brew test`: fresh Homebrew installs do not yet have its optional
# formula-test gems (including minitest). Use Homebrew's pinned Bundler group.
Utils::GemSetup.install_bundler_gems!(groups: ["formula_test"], setup_path: false)
require "formula_assertions"

raise "expected formula name, formula path, source directory, spec, archive and Go executable" unless ARGV.length == 6
formula_name = ARGV.fetch(0)
formula_path = Pathname(ARGV.fetch(1)).realpath
source = Pathname(ARGV.fetch(2)).realpath
spec = ARGV.fetch(3).to_sym
raise "expected head or stable formula spec" unless [:head, :stable].include?(spec)

Dir.mktmpdir("irrigator-homebrew-") do |temporary|
  prefix_path = Pathname(temporary)/"prefix"
  # Homebrew resolves only registered formula names. Loading an arbitrary source
  # path is deliberately unsupported; use the owned tap's explicitly trusted name.
  formula = Formulary.factory(formula_name, spec)
  raise "Homebrew loaded a different formula" unless formula.path.realpath == formula_path
  formula.extend(Homebrew::Assertions)
  formula.build = BuildOptions.new(Options.create(spec == :head ? ["--HEAD"] : []), formula.options)
  raise "expected Irrigator formula" unless formula.name == "irrigator"
  raise "HEAD recipe must declare its Go build dependency" if spec == :head && formula.deps.none? { |dep| dep.name == "go" && dep.build? }
  raise "stable archives must not require Go" if spec == :stable && formula.deps.any? { |dep| dep.name == "go" }
  if spec == :stable
    raise "stable recipe checksum differs from candidate" unless formula.stable.checksum.to_s == Digest::SHA256.file(ARGV.fetch(4)).hexdigest
  end
  formula.define_singleton_method(:prefix) { |*_args| prefix_path }
  formula.buildpath = source
  ENV.activate_extensions!(env: "std")
  # brew ruby starts with a system-only PATH. A normal install supplies its Go
  # dependency; this private recipe check uses the caller's existing compiler.
  ENV.prepend_path "PATH", Pathname(ARGV.fetch(5)).dirname if spec == :head
  Dir.chdir(source) { formula.install }
  raise "recipe did not install the executable" unless formula.bin.join("irrigator").executable?
  formula.run_test
  puts "PASS: Homebrew #{spec} recipe installed Irrigator in a private prefix and passed its mock test."
end
