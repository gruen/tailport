class Tailport < Formula
  desc "TUI to expose local ports across your tailnet via tailscale serve"
  homepage "https://github.com/gruen/tailport"
  url "https://github.com/gruen/tailport/archive/refs/tags/v0.2.5.tar.gz"
  sha256 "a3cc70fd366e24f59da79d3a3c48572179817f9bed0bfcdee874711918d305bf"
  license "MIT"
  head "https://github.com/gruen/tailport.git", branch: "main"

  depends_on "go" => :build
  # tailport shells out to the tailscale CLI for serve/funnel. Port discovery
  # uses lsof on macOS (ships with the OS) and ss on Linux (iproute2, present on
  # any Linuxbrew host) -- neither is a formula dependency.
  depends_on "tailscale"

  def install
    # -X main.version is required, not cosmetic: this builds from a release
    # tarball, which carries no VCS metadata, so the module-info fallback in
    # cmd/tailport/main.go resolves to "(devel)" and the binary would otherwise
    # report "dev". std_go_args supplies -trimpath and -o bin/"tailport".
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{version}"), "./cmd/tailport"
  end

  test do
    assert_match "tailport #{version}", shell_output("#{bin}/tailport --version")
  end
end
