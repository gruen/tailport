class Tailport < Formula
  desc "TUI to share local ports on your tailnet or publicly (Funnel, Caddy, Cloudflare)"
  homepage "https://github.com/gruen/tailport"
  url "https://github.com/gruen/tailport/archive/refs/tags/v0.3.5.tar.gz"
  sha256 "5208a3e4bd0aed022cdae119ba9c86953057105669eb29cadd998a9c34f0a464"
  license "MIT"
  head "https://github.com/gruen/tailport.git", branch: "main"

  depends_on "go" => :build
  # tailport shells out to the tailscale CLI for serve/funnel, but that's a
  # RUNTIME tool found on PATH, not a build/install dependency (xzgh): tailport
  # runs and discovers ports without it (lsof on macOS, which ships with the OS;
  # ss on Linux), degrading only the serve/funnel actions -- every tailscale
  # call captures its own not-found error rather than crashing. Deliberately NOT
  # `depends_on "tailscale"`: on macOS most people run the Tailscale app (App
  # Store / standalone), whose bundled CLI a hard formula dep would duplicate
  # and whose daemon the formula's `tailscaled` would fight; and the `tailscale`
  # bottle isn't published for every macOS tier, which broke `brew install
  # tailport` outright on a Tier-3 runner. The caveat below points users at
  # Tailscale instead. (Linux packaging keeps the dep -- `tailscale` is a real,
  # always-available package there.)

  def install
    # -X main.version is required, not cosmetic: this builds from a release
    # tarball, which carries no VCS metadata, so the module-info fallback in
    # cmd/tailport/main.go resolves to "(devel)" and the binary would otherwise
    # report "dev". std_go_args supplies -trimpath and -o bin/"tailport".
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{version}"), "./cmd/tailport"
  end

  def caveats
    <<~EOS
      tailport exposes your local ports over your tailnet using the `tailscale`
      CLI (`tailscale serve` / `tailscale funnel`), so install Tailscale to use
      it: https://tailscale.com/download (the macOS app bundles the CLI), or
      `brew install tailscale`.
    EOS
  end

  test do
    assert_match "tailport #{version}", shell_output("#{bin}/tailport --version")
  end
end
