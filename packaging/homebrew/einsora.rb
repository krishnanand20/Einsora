# Einsora's Homebrew formula (ADR-0023). Its source is packaging/homebrew/einsora.rb in
# github.com/krishnanand20/Einsora: for each release, the release workflow fills in the source
# archive and its SHA-256 (packaging/homebrew/formula.sh) and publishes it to
# krishnanand20/homebrew-tap. Homebrew builds it from source, as it builds its own formulae, so it
# runs without notarization: Homebrew doesn't quarantine what it builds.
class Einsora < Formula
  desc "Free, self-hosted GPU cloud for your Macs and Linux machines"
  homepage "https://einsora.com"
  url "@URL@"
  sha256 "@SHA256@"
  license "MIT"

  depends_on "go" => :build
  depends_on "node" => :build

  on_macos do
    depends_on arch: :arm64 # Apple Silicon only (ADR-0013)
  end

  def install
    ENV["CGO_ENABLED"] = "0"
    # The dashboard is built into the binary, as `make web-embed` does.
    cd "web" do
      system "npm", "ci"
      system "npm", "run", "build"
    end
    (buildpath/"controlplane/internal/webui/dist").install Dir["web/dist/*"]
    cd "controlplane" do
      system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{version}"), "./cmd/einsora"
    end
  end

  test do
    assert_match "einsora #{version} ", shell_output("#{bin}/einsora version")
  end
end
