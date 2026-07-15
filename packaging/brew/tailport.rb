class Tailport < Formula
  desc "TUI to expose local ports across your tailnet via tailscale serve"
  homepage "https://github.com/gruen/tailport"
  url "https://github.com/gruen/tailport/archive/refs/tags/v0.1.6.tar.gz"
  sha256 "299ff594cd5610d469297888718c2c679b416432bd80fa635c4fd69cb963f2a4"
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
