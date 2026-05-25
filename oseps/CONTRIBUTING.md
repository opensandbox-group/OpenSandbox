# OSEP (OpenSandbox Enhancement Proposals)

Use this directory to draft, review, and store enhancement proposals before they
undergo broader discussion.

> [!NOTE]
> The OSEP process and template structure is inspired by
> [Tekton Enhancement Proposals (TEPs)](https://github.com/tektoncd/community/tree/main/teps).

> [!IMPORTANT]
> **When is an OSEP required?**
>
> Use the OSEP process for changes that:
> - Introduce new features or major enhancements
> - Modify the core sandbox API or runtime behavior
> - Affect the security model or isolation guarantees
>
> Small bug fixes, documentation updates, and minor refactors can be submitted
> directly as Pull Requests without an OSEP.

## Getting started

1. Run the init script to create a new proposal:

   ```bash
   oseps/init-osep.sh "Proposal Title"
   ```

   This copies the template, fills in metadata, and creates a sequentially
   numbered `0001-proposal-title.md` draft.

2. Fill in each section from the template (`Summary`, `Motivation`, …).
3. Once ready, submit the resulting file in a PR for community review.

**Available options:**

```bash
oseps/init-osep.sh --help
oseps/init-osep.sh --status provisional --author "@username" "My Feature"
```

## Template

The template used for new proposals lives at `oseps/osep-template.md.template`
and mirrors Tekton's TEP structure while capturing the key sections needed
for OpenSandbox planning. Each generated file starts with YAML front matter
followed by the title and TOC:

```yaml
---
title: My First Proposal
authors:
  - "@your-github-handle"
creation-date: 2025-12-21
last-updated: 2025-12-21
status: draft
---

# OSEP-0001: My First Proposal

<!-- toc -->
- [Summary](#summary)
...
<!-- /toc -->
```

This YAML front matter renders as a table on GitHub and keeps the proposal
metadata (status, authors, dates) visible at the top of the document.

## Status lifecycle

| Status | Description |
|--------|-------------|
| `draft` | Work in progress; not yet under formal review. |
| `provisional` | Maintainers agree with the direction; design details still pending. |
| `implementable` | Design approved and compliance checks passed; ready for implementation. |
| `implementing` | Code is being merged and SDKs are being synchronized. |
| `implemented` | Feature has reached GA status with complete documentation. |
| `withdrawn` | Author has withdrawn the proposal. |
| `rejected` | Maintainers have declined the proposal. |
