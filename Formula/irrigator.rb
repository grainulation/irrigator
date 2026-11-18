class Irrigator < Formula
  desc "Durable local workflows for coding agents"
  homepage "https://irrigator.dev"
  license "MIT"

  # BEGIN GENERATED STABLE RELEASE
  # Frozen runtime commit: e8a3b57bf9a129aa221e439e9d1b716649ca94d7
  # Source SHA-256: 189e8ec0c3f7e52e1b8eadc6cb28ea48dff4e848d2ff09394adda18f065aebf7
  version "1.0.0"

  on_macos do
    on_arm do
      url "https://github.com/grainulation/irrigator/releases/download/v1.0.0/irrigator-1.0.0-darwin-arm64.tar.gz"
      sha256 "78591ea8be9cd2774bd7848d61193639244d921ede8519ac4eb37df5ef8f3da3"
    end
    on_intel do
      url "https://github.com/grainulation/irrigator/releases/download/v1.0.0/irrigator-1.0.0-darwin-amd64.tar.gz"
      sha256 "af0f5fa1c5abde9316f00e266e0b49162b3af0993eaf42eb4e418d5e7e342474"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/grainulation/irrigator/releases/download/v1.0.0/irrigator-1.0.0-linux-arm64.tar.gz"
      sha256 "c471854c06f5e1e6aff1c438d3f67b487d93c82bcbc921cbf9132001b7af3c02"
    end
    on_intel do
      url "https://github.com/grainulation/irrigator/releases/download/v1.0.0/irrigator-1.0.0-linux-amd64.tar.gz"
      sha256 "e353c71f8fe0415c96f749d17be7cf287f3efd06fdf11d9e76a8077a78f260df"
    end
  end
  # END GENERATED STABLE RELEASE

  head do
    url "https://github.com/grainulation/irrigator.git", branch: "main"
    depends_on "go" => :build
  end

  def install
    if build.head?
      ENV["GOTOOLCHAIN"] = "local"
      ENV["GOWORK"] = "off"
      ENV["CGO_ENABLED"] = "0"
      ENV["GOFLAGS"] = ""
      system "go", "build", "-buildvcs=false", *std_go_args, "."
    else
      bin.install "irrigator"
      pkgshare.install "provenance.json", "SHA256SUMS"
    end
  end

  test do
    ENV["IRRIGATOR_LAUNCH_CONTEXT"] = ""
    ENV["IRRIGATOR_EXECUTOR"] = "local"
    ENV["IRRIGATOR_UI"] = "plain"
    ENV["IRRIGATOR_SESSION_REGISTRY"] = (testpath/"session.json").to_s
    reported_version = shell_output("#{bin}/irrigator version")
    if build.head?
      assert_match(/\A\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?\n\z/, reported_version)
    else
      assert_equal "#{version}\n", reported_version
    end
    system bin/"irrigator", "demo", "--run", testpath/"demo"
    system bin/"irrigator", "export", testpath/"demo", "--out", testpath/"results.json"
    assert_path_exists testpath/"results.json"
  end
end
