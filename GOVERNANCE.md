# OpenSandbox Governance

This document describes how OpenSandbox is governed today and how technical
decisions are made in the project.

It is intended to reflect the project's current open development practice. As
OpenSandbox grows, this document may evolve to support a more formal governance
model.

## Goals

OpenSandbox governance is designed to keep the project:

- open to contributors and users
- pragmatic in day-to-day decision making
- transparent in technical direction
- safe for public API, runtime, and security-sensitive changes
- sustainable across multiple components and maintainers

## Scope

This document applies to the OpenSandbox repository and its major public
surfaces, including:

- public APIs and specifications in `specs/`
- the lifecycle server in `server/`
- runtime components in `components/`
- Kubernetes controller and related assets in `kubernetes/`
- SDKs in `sdks/`
- CLI, examples, and project documentation

## Project Values

OpenSandbox maintainers and contributors are expected to act consistently with
these principles:

- **Open development**: design discussion, issue tracking, and code review
  happen in public whenever possible.
- **Compatibility awareness**: public APIs, SDKs, CLI behavior, and documented
  user workflows should not change casually.
- **Security first**: changes affecting isolation, networking, credentials, or
  execution safety require extra scrutiny.
- **Component ownership with cross-project accountability**: subsystem
  maintainers own their areas, while cross-cutting changes require broader
  review.
- **Documentation and implementation alignment**: public contracts, code, SDKs,
  examples, and docs should stay consistent.

## Roles

### Contributors

Contributors are anyone who participates in the project, including by:

- opening issues or discussions
- submitting pull requests
- reviewing code
- improving docs, tests, examples, or tooling

Contributors are expected to follow:

- [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)
- [CONTRIBUTING.md](CONTRIBUTING.md)
- [SECURITY.md](SECURITY.md) for vulnerability reporting

### Maintainers

Maintainers are contributors entrusted with reviewing and guiding changes in one
or more parts of the repository.

Today, the public record of subsystem maintainership is the
[`CODEOWNERS`](.github/CODEOWNERS) file. Code owners for a given path are the
default maintainers for that area.

Maintainer responsibilities include:

- reviewing pull requests for owned areas
- helping preserve code quality, compatibility, and security
- requesting cross-component review when a change affects other surfaces
- keeping specs, implementation, tests, docs, and examples aligned when needed
- helping contributors land changes successfully

### Project Maintainers

Project Maintainers are the maintainers responsible for repository-wide
direction and final technical decisions when a matter cannot be resolved within
normal component review.

Until a dedicated `MAINTAINERS.md` is added, the fallback `*` owners in
[`CODEOWNERS`](.github/CODEOWNERS) are treated as the public list of current
Project Maintainers.

Project Maintainers are responsible for:

- cross-cutting technical decisions
- governance updates
- maintainer onboarding and offboarding
- resolving review deadlocks
- ensuring major changes follow the appropriate design process

## Decision Making

### Day-to-Day Changes

Most changes are made through the normal pull request workflow described in
[CONTRIBUTING.md](CONTRIBUTING.md):

1. discuss the change in an issue when appropriate
2. submit a pull request
3. pass automated checks
4. receive maintainer review for affected areas
5. address feedback
6. merge once approved

Normal changes are decided by maintainer review and rough consensus.

### Component-Level Decisions

For changes limited to one subsystem, the maintainers of that subsystem are the
primary decision makers. Their review should carry the most weight for:

- implementation details
- code structure
- tests
- operational behavior within that component

If a change affects multiple subsystems, the relevant maintainers should be
involved before merge.

### Cross-Cutting and Sensitive Decisions

Changes with broader impact require wider review. This includes changes to:

- public APIs or schemas in `specs/`
- SDK interfaces or generated outputs
- CLI behavior
- lifecycle semantics
- runtime isolation or security guarantees
- ingress, egress, or authentication behavior
- release processes or repository-wide tooling

For these changes, maintainers should seek explicit review from all materially
affected areas, not just the first area touched by the patch.

### Major Design Changes and OSEPs

OpenSandbox uses the OpenSandbox Enhancement Proposal process (`OSEP`) for major
changes.

An OSEP is expected for changes that:

- introduce major features or architectural changes
- modify the core API or runtime behavior
- affect the security model or isolation guarantees

The public OSEP process is documented in:

- [oseps/README.md](oseps/README.md)
- [oseps/CONTRIBUTING.md](oseps/CONTRIBUTING.md)

When an OSEP is required, implementation should follow the approved design
direction rather than bypassing it through code review alone.

### Consensus and Voting

OpenSandbox prefers **lazy consensus** for most technical decisions:

- if affected maintainers agree, the change may proceed
- if concerns are raised, they should be addressed in the PR, issue, or OSEP
  discussion

If consensus cannot be reached in a reasonable time, Project Maintainers may use
a simple majority vote of participating Project Maintainers.

Voting rules:

- each participating Project Maintainer has one vote
- maintainers should recuse themselves in case of direct conflicts of interest
- if a vote ties, the proposal does not pass and the status quo remains

## Reviews and Merge Expectations

The following expectations apply before merge:

- relevant CI checks should pass, or failures must be understood and accepted by
  maintainers
- at least one maintainer of the affected area should review the change
- cross-cutting changes should be reviewed by all materially affected areas when
  practical
- breaking changes should be clearly called out, with migration guidance where
  needed
- docs and tests should be updated when behavior changes

Maintainers may decline or defer a change if it:

- conflicts with approved design direction
- introduces unnecessary compatibility risk
- weakens security or isolation without a strong justification
- mixes unrelated work into a single change

## Public Interfaces and Compatibility

OpenSandbox treats several surfaces as public contracts:

- API specifications in `specs/`
- published SDKs
- CLI behavior
- documented configuration and deployment behavior

Maintainers should prefer additive and backward-compatible changes when
possible.

When changing a public contract, maintainers should update or verify the
affected implementation, SDKs, docs, examples, and release outputs in the same
change when practical.

## Releases

Releases are managed through the public workflows and scripts in this
repository, including the GitHub Actions workflows under `.github/workflows/`
and the release tooling documented in `docs/release-automation.md`.

Maintainers responsible for a release target are expected to ensure that:

- the target has appropriate validation
- release notes accurately reflect user-visible changes
- versioning and tags follow the documented release conventions

## Communication Channels

The project's public collaboration channels are:

- GitHub Issues for bugs, feature requests, and implementation questions
- GitHub Discussions for broader design discussion and community help
- pull requests for concrete code and documentation review
- OSEPs for major design work

Security issues should follow the private reporting guidance in
[SECURITY.md](SECURITY.md).

## Becoming a Maintainer

New maintainers are selected based on sustained, high-quality contribution to
the project.

Signals that someone may be ready for maintainership include:

- repeated high-quality code or documentation contributions
- strong reviews and constructive technical feedback
- reliable follow-through on owned work
- good judgment on compatibility, security, and project direction
- collaborative behavior with contributors and maintainers

The typical process is:

1. nomination by an existing Project Maintainer
2. discussion among Project Maintainers
3. no unresolved objections after a reasonable review period, or approval by
   majority vote if needed
4. update of `CODEOWNERS` and any other relevant public maintainer records

## Maintainer Inactivity and Removal

Maintainers may step down at any time by notifying the project.

Project Maintainers may also update maintainer status when someone has been
inactive for an extended period, for example several months without meaningful
review or maintenance activity.

Removal should be handled respectfully and pragmatically, with the goal of
keeping ownership accurate rather than punitive.

Maintainers may also be removed for serious violations of project expectations,
including repeated abuse of project privileges or violations of the Code of
Conduct.

## Governance Changes

Changes to this document should be made through a public pull request.

Governance changes should receive review from Project Maintainers and should not
be merged without giving maintainers and contributors a reasonable opportunity
to comment.

Substantial governance changes may be proposed through an OSEP or a dedicated
governance discussion if maintainers believe broader review is warranted.
