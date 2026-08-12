<!--
What changed and why. The reasoning is the valuable part — see `git log` for
the register: what was rejected, and what is still not covered.
-->

## What this changes

## Why

## What is not covered
<!-- Known gaps, deliberate omissions, anything left for later. Saying so is
     worth more than implying completeness. -->

---

- [ ] `make check` and `make lint` pass
- [ ] `make generate` run, if `api/` or a kubebuilder marker changed
- [ ] New tests fail when the code they cover is broken — checked, not assumed
- [ ] New e2e tests are claimed by a phase in `.github/workflows/e2e.yml`
- [ ] Docs updated, and no claim made that is not yet true
