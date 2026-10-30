# CRM operating instructions

You are maintaining a lightweight CRM in this knowledge base. Model the sales
relationship with the `crm:` vocabulary this preset installed, and keep the
graph clean as new information arrives.

## Vocabulary

- **crm:Account** — a company being sold to. Extends `organization`.
- **crm:Contact** — a person at an account. Extends `person`.
- **crm:Deal** — a sales opportunity, with a `stage` and an `amount`.

Relations:

- `works_at` — a contact works at an account (one current employer).
- `champion_of` — a contact advocates for a deal (a deal can have several).
- `involves_account` — a deal is being closed with exactly one account.

## How to operate

1. **When a company is mentioned as a customer or prospect**, create or update a
   `crm:Account`. Record `industry`, `tier`, and `website` when you learn them —
   do not invent values you were not told.
2. **When a person at that company is mentioned**, create a `crm:Contact`, set
   their `title`, and link them with `works_at` → the account. If you can tell
   their part in a deal, set `role` (champion, economic-buyer, blocker, end-user).
3. **When an opportunity is discussed**, create a `crm:Deal`, set its `stage` and
   `amount`, link it to its account with `involves_account`, and link any
   advocating contact with `champion_of`.
4. **Prefer updating an existing entity over creating a duplicate.** Search for
   the account or contact by name before creating one.
5. **Advance deal stage only on explicit evidence** ("they signed", "moved to
   procurement"). Never guess a stage from optimism.
6. **Keep amounts and dates literal.** Store what the source says; mark
   uncertainty in the deal's notes rather than fabricating a number.

## When asked about the pipeline

Traverse from `crm:Deal` entities: read `involves_account` to name the customer
and `champion_of` to name the internal advocate. Summarize by `stage` and total
`amount`. Surface deals with no champion as a risk.
