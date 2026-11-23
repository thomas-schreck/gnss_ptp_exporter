# Governance

gnss_ptp_exporter is a small open-source project with a single maintainer.

## Roles

**Maintainer**: Thomas Schreck ([@thomas-schreck](https://github.com/thomas-schreck)).
The maintainer sets the direction, triages issues, reviews and merges pull
requests, cuts releases and handles security reports, and has the final
say on all decisions.

**Contributors**: anyone who opens an issue, comments or submits a pull
request. Contributors keep the copyright of their work and license it under
the project's MIT License by submitting it.

## Changes

1. **Issue first.** Every change except typo fixes is tracked by an issue
   before work starts.
2. **Pull request.** One pull request per issue; the description starts
   with `Closes #<issue>`.
3. **Review.** The maintainer reviews every pull request, including their
   own changes. Automated review agents may assist; their findings are
   input, approval and merge are a human decision. CI must pass.
4. **Merge.** Squash or rebase merge into `main` (linear history).

## AI-assisted contributions

AI coding assistants may be used. The submitter is responsible for the
result as if they had written it: disclose substantial AI assistance (a
`Co-Authored-By` trailer is enough), understand every change, and make sure
no code with an incompatible license is introduced.

## Releases

- Versions follow [Semantic Versioning](https://semver.org). Before 1.0,
  metric changes may happen in minor versions and are listed in the
  changelog.
- The maintainer updates `CHANGELOG.md`, tags `main` and pushes the tag:
  `git tag -a vX.Y.Z -m "gnss_ptp_exporter X.Y.Z" && git push origin vX.Y.Z`
  (a tag with a suffix such as `v0.2.0-rc.1` becomes a pre-release).
- The tag starts the *Release* workflow: tests, linux amd64/arm64/armv7
  archives, SPDX SBOM, SHA256SUMS, build provenance attestation and the
  GitHub release.

## Security

Security issues are not reported in public issues. See [SECURITY.md](SECURITY.md).

## Conduct

Everyone participating follows the [Code of Conduct](CODE_OF_CONDUCT.md).
