# OpenSandbox Governance

The [Technical Charter](CHARTER.md) establishes the project's technical governance. This document records project-specific working practices and maintainer procedures under that Charter.

## Governance Documents

- [CHARTER.md](CHARTER.md) defines the mission, TSC authority, formal voting rules, project policies, and amendment requirements.
- [CONTRIBUTING.md](CONTRIBUTING.md#project-roles-and-technical-steering-committee) defines project roles and explains contribution workflows and communication channels.
- [MAINTAINERS.md](MAINTAINERS.md) lists current maintainers; [CODEOWNERS](.github/CODEOWNERS) records path ownership and routes reviews.
- Conduct and vulnerability reports follow [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) and [SECURITY.md](SECURITY.md), respectively.

## Decision Making

### Reviews and Proposals

Day-to-day changes follow the [pull request process](CONTRIBUTING.md#pull-request-process). Component maintainers lead decisions in their areas; changes affecting multiple components should involve the materially affected maintainers.

Changes to public APIs, SDKs, CLI behavior, lifecycle semantics, runtime isolation, ingress/egress, authentication, release processes, or repository-wide tooling require wider review. Maintainers should seek explicit review from all materially affected areas.

Before merge:

- Relevant CI checks should pass, or failures must be understood and accepted by maintainers.
- At least one maintainer of the affected area should review the change.
- Prefer additive, backward-compatible changes to public APIs, SDKs, CLI behavior, and documented configuration and deployment behavior. Call out breaking changes and provide migration guidance where needed.
- Keep affected specifications, implementation, tests, documentation, examples, and release outputs aligned.

Maintainers may decline changes that conflict with approved design, introduce unnecessary compatibility risk, weaken security without sufficient justification, or mix unrelated work.

Major features, architectural changes, core API or runtime behavior changes, and changes to the security model or isolation guarantees follow the [OSEP process](oseps/CONTRIBUTING.md). Implementation should follow the approved design.

### Consensus and Voting

OpenSandbox prefers **lazy consensus** for day-to-day technical decisions. A change may proceed when affected maintainers agree; concerns should be addressed in the PR, issue, or OSEP discussion.

If consensus cannot be reached in a reasonable time, Project Maintainers may hold a TSC vote under [Section 3 of the Technical Charter](CHARTER.md#3-tsc-voting). License exceptions and Charter amendments follow the special requirements in [Sections 7.c.](CHARTER.md#7-intellectual-property-policy) and [8.a.](CHARTER.md#8-amendments), respectively.

Maintainers should recuse themselves in cases of direct conflicts of interest. Maintainer nominations and promotions follow the dedicated electorate and approval thresholds in [Becoming a Maintainer](#becoming-a-maintainer).

## Releases

Maintainers responsible for releases are expected to ensure appropriate validation, accurate release notes, and adherence to the documented versioning and tag conventions. Release procedures are maintained in [Release Automation](docs/community/release-automation.md).

## Becoming a Maintainer

OpenSandbox uses an explicit advancement process for contributors seeking increased stewardship in the project. This process is adapted from the [containerd project governance](https://github.com/containerd/project/blob/main/GOVERNANCE.md) to match OpenSandbox's existing roles: Contributor, Maintainer (component ownership), and Project Maintainer (repository-wide stewardship).

Adopting this process retains all existing Maintainers and Project Maintainers recorded in [`CODEOWNERS`](.github/CODEOWNERS).

### Expectations and Readiness

Maintainership reflects demonstrated trust, consistent follow-through, and commitment to the project's long-term health. There is no fixed pull request or commit count, and contribution volume alone does not guarantee promotion.

Candidates for either **Maintainer** (component scope) or **Project Maintainer** (repository-wide scope) are expected to show:

- More than three months of meaningful, sustained involvement in the project.
- Varied contributions across the project or target component, including code, code reviews, issue triage, documentation, tests, or community support.
- Sound technical judgment, particularly regarding backward compatibility, security, error handling, and test quality.
- Collaborative communication and constructive reviews with other contributors.

In addition, candidates for **Project Maintainer** are expected to show:

- Sustained maintenance and leadership across the project.
- Sound cross-component and project-wide technical judgment.
- Active stewardship across multiple subsystems, release health, and governance.

### Nomination Process

Advancement is proposed through a public pull request updating [`CODEOWNERS`](.github/CODEOWNERS) and [MAINTAINERS.md](MAINTAINERS.md):

1. **Sponsorship**: Any existing Maintainer or Project Maintainer may sponsor a candidate by opening a nomination pull request against `CODEOWNERS` (adding the candidate to specific component paths for Maintainer, or to the fallback `*` list for Project Maintainer). Contributors interested in nomination may ask an existing Maintainer in a public issue or discussion.
2. **Nomination PR Content**: The pull request description must specify the candidate's GitHub username, the target role and path scope, links to their contributions demonstrating readiness, the list of eligible voting Project Maintainers, and the required approval count.
3. **Candidate Acceptance**: The candidate must comment on the pull request explicitly accepting the nomination. Candidate acceptance confirms their commitment to the role's responsibilities; it is not counted as an approval vote.

### Discussion and Voting

Nominations are decided through public review and voting on the pull request:

- **Discussion and Voting Period**: The nomination pull request must remain open for at least seven full calendar days from opening. Reaching the approval threshold early does not waive or shorten this period.
- **Eligible Electorate**: The voting electorate consists of all current Project Maintainers recorded in the fallback `*` rule of [`CODEOWNERS`](.github/CODEOWNERS) on the base branch at the time the nomination pull request is opened. Each eligible Project Maintainer has one vote. Candidates cannot vote on their own nomination, and candidate acceptance is not counted as an approval vote.
- **Approval Thresholds**:
  - **Maintainer (component scope)**: Requires affirmative approvals (pull request approval or explicit LGTM) from at least **one-third (1/3)** of the eligible Project Maintainers, rounded up to the nearest whole maintainer.
  - **Project Maintainer**: Requires affirmative approvals from at least **two-thirds (2/3)** of the eligible Project Maintainers, rounded up to the nearest whole maintainer.
- **Voting Rules**: Approvals must be affirmative. Maintainers with direct conflicts of interest should recuse themselves. Abstentions, non-responses, or recusals do not count as approvals and do not lower the denominator. These nomination thresholds govern the role-specific advancement process; formal TSC votes follow [Consensus and Voting](#consensus-and-voting) and the Technical Charter.
- **Public Objections**: Any objections or concerns should be discussed publicly in the pull request during the seven-day period. There is no unanimity requirement or individual veto beyond the specified vote.

### Decision and Onboarding

- After the full seven calendar days have passed, a Project Maintainer verifies that the approval threshold has been met and that the candidate has explicitly accepted the nomination. The Project Maintainer then merges the pull request and coordinates the necessary scoped repository permissions.
- If the threshold is not met, the pull request cannot be merged and is closed without promotion. There is no automatic promotion based solely on time or contribution counts.
- Keep [MAINTAINERS.md](MAINTAINERS.md) and [`CODEOWNERS`](.github/CODEOWNERS) synchronized when recording the appointment.

## Maintainer Inactivity and Removal

Maintainers may step down at any time by notifying the project.

Project Maintainers may also update maintainer status when someone has been inactive for an extended period, for example several months without meaningful review or maintenance activity.

Removal should be handled respectfully and pragmatically, with the goal of keeping ownership accurate rather than punitive.

Maintainers may also be removed for serious violations of project expectations, including repeated abuse of project privileges or violations of the Code of Conduct.

## Governance Changes

Changes to this document should be made through a public pull request.

Governance changes should receive review from Project Maintainers and should not be merged without giving maintainers and contributors a reasonable opportunity to comment.

Substantial governance changes may be proposed through an OSEP or a dedicated governance discussion if maintainers believe broader review is warranted.

Changes to the Technical Charter itself follow [Section 8 of the Charter](CHARTER.md#8-amendments). Review or merge of a pull request alone does not amend the Charter.
