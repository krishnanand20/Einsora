# 0011. Free for everyone: no pricing, no billing

> **Update (ADR-0020):** the licence this ADR left open is decided: MIT. The product is now named
> TensoraFi. Everything else here still applies.

- **Status:** Accepted
- **Date:** 2026-10-23
- **Supersedes:** the billing parts of ADR-0006, and the pricing, payment, and "first region" decisions in the product spec's decision log

## Context

Tensora is for everyone, including students, developers, researchers, startups, small companies,
enterprises, and big tech. Anyone can install it on their own machines. Earlier documents assumed a
paid cloud we operate, with USD pricing, payments, invoices, sales tax, and a first region in India.

## Decision

- **Tensora is completely free.** There is no pricing, no paid tier, no payment flow, no invoice, and no paywalled feature anywhere in the product, code, or docs. Enterprise features (SSO, audit logs, regions, HA) are free as well.
- **Usage accounting stays; billing goes.** The allocation-based mechanism from ADR-0006 (lifecycle events → usage records) is kept for quotas, fair-share scheduling, reports, and recognizing hosts who share machines. It never turns into money.
- **The product is software, not a hosted service.** Anyone can run an instance, privately or as a shared pool (ADR-0014). The project doesn't have to operate a cloud.
- **Distribution:** published as open source. Apache-2.0 is recommended (permissive, with a patent grant, and easy for companies to adopt). Choosing the licence is an open decision in the roadmap.

## Consequences

- Payment providers, tax engines, invoices, price books, and payment-related KYC are removed from all plans.
- "Billing" becomes "usage accounting" everywhere: AGENTS.md rules 5 and 10, `docs/architecture/usage-accounting.md` (renamed from metering-and-billing.md), and in code `State.Billable` → `State.Metered`.
- Without payment as a barrier, abuse control relies on verified accounts, quotas, rate limits, and reputation (ADR-0014).
- No business model funds maintainers or shared hardware. That's outside the product's scope and isn't solved here.

## Alternatives considered

- **Open core** (paid enterprise features): rejected, because the product must be completely free.
- **Freemium hosted cloud:** rejected for the same reason. Hosting is something anyone can do with the free software.
