#!/usr/bin/env bash
# homebrew-formula.sh — render the Homebrew formula for a published release.
#
# The tap (github.com/mo-cmyk/homebrew-mazarbul) is generated, never edited by
# hand: release-please.yml runs this after the release archives are attached
# and commits the output as Formula/maz.rb in the tap.
#
# Usage: homebrew-formula.sh <tag> <checksums.txt>   (prints the formula)

set -euo pipefail

tag="${1:?usage: homebrew-formula.sh <tag> <checksums.txt>}"
sums="${2:?usage: homebrew-formula.sh <tag> <checksums.txt>}"
version="${tag#v}"
base="https://github.com/mo-cmyk/mazarbul/releases/download/${tag}"

sha() {
	local f="mazarbul_${tag}_$1.tar.gz" s
	s=$(awk -v f="$f" '$2==f {print $1; exit}' "$sums")
	[ -n "$s" ] || { echo "ERROR: no checksum for $f in $sums" >&2; exit 1; }
	printf '%s' "$s"
}

cat <<RUBY
class Maz < Formula
  desc "Local-first system of records for the era of agents"
  homepage "https://github.com/mo-cmyk/mazarbul"
  version "${version}"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "${base}/mazarbul_${tag}_darwin_arm64.tar.gz"
      sha256 "$(sha darwin_arm64)"
    end
    on_intel do
      url "${base}/mazarbul_${tag}_darwin_amd64.tar.gz"
      sha256 "$(sha darwin_amd64)"
    end
  end

  on_linux do
    on_arm do
      url "${base}/mazarbul_${tag}_linux_arm64.tar.gz"
      sha256 "$(sha linux_arm64)"
    end
    on_intel do
      url "${base}/mazarbul_${tag}_linux_amd64.tar.gz"
      sha256 "$(sha linux_amd64)"
    end
  end

  def install
    # maz resolves its bundled LadybugDB engine via @loader_path/../lib
    # (\$ORIGIN/../lib on Linux), so the lib must sit beside bin/.
    bin.install "maz"
    lib.install Dir["lib/*"]
    man1.install Dir["man/man1/*.1"]
    bash_completion.install "completions/maz.bash" => "maz"
    zsh_completion.install "completions/maz.zsh" => "_maz"
    fish_completion.install "completions/maz.fish"
  end

  def caveats
    <<~EOS
      maz needs a model provider. For local models:
        brew install ollama && ollama serve
        maz init
      Or use a hosted provider:
        maz init --provider anthropic --api-key ...

      If the daemon runs under launchd (maz daemon install), re-run
        maz daemon install
      after every brew upgrade: macOS will not start a replaced binary
      under the old registration (maz doctor flags this as daemon.launchd).
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/maz --version")
  end
end
RUBY
