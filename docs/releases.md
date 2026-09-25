# Releases and Homebrew

The tag-triggered [release workflow](../.github/workflows/release.yml) runs
GoReleaser to build KEDR for macOS, Linux, and Windows on amd64 and arm64, upload
archives, checksums, and SBOMs to GitHub Releases, and publish a Homebrew cask.

## One-time setup

1. The source repository must be available at `kedify/kedr` on GitHub with Actions
   enabled. Homebrew downloads its binaries from that repository's public release
   assets.
2. Reuse the existing public [kedify/homebrew-tap](https://github.com/kedify/homebrew-tap)
   repository. A tap is a Git repository containing Homebrew package definitions;
   one tap can hold multiple packages. No separate tap just for KEDR is needed.
3. Create a fine-grained GitHub personal access token with `kedify` as the resource
   owner, access to only `homebrew-tap`, and **Contents: Read and write**. Add it as
   the `HOMEBREW_TAP_TOKEN` Actions secret in `kedify/kedr`:

   ```sh
   gh secret set HOMEBREW_TAP_TOKEN --repo kedify/kedr
   ```

   This command prompts for the token. The workflow's automatic `GITHUB_TOKEN`
   publishes KEDR's release, but cannot write to the separate tap repository.
   GoReleaser uses `HOMEBREW_TAP_TOKEN` only for the tap. The workflow checks that
   this secret is present before starting the release.

## Publishing

Push a version tag, such as `v0.1.0`, on the commit to release. GoReleaser generates
`Casks/kedr.rb`, including download URLs and SHA-256 checksums, and commits it to
the tap's default branch. The existing `kedify` formula stays in place. Prerelease
tags such as `v0.1.0-rc1` create GitHub prereleases and do not update the tap.

Once the first stable release has published the cask, users can install it with:

```sh
brew install --cask kedify/tap/kedr
kedr version
```

Subsequent stable releases update the same cask; users upgrade with
`brew upgrade --cask kedr`.

The [GoReleaser configuration](../.goreleaser.yaml) uses
[`homebrew_casks`](https://goreleaser.com/customization/publish/homebrew_casks/),
the supported integration for prebuilt binaries. It includes the documented
macOS quarantine hook for unsigned binaries, scoped to the installed `kedr`
executable. The release workflow installs Syft for the existing SBOM step.

## Local validation

With GoReleaser and Syft installed:

```sh
goreleaser check
goreleaser release --snapshot --clean
```

Snapshot mode builds archives and renders the cask locally without publishing a
release or modifying the tap, and does not require publishing credentials. Inspect
`dist/homebrew/Casks/kedr.rb` before publishing.

Homebrew documents tap repositories in
[How to Create and Maintain a Tap](https://docs.brew.sh/How-to-Create-and-Maintain-a-Tap).
GoReleaser documents the separate-token requirement under
[GitHub Actions](https://goreleaser.com/customization/publish/homebrew_casks/#github-actions).
