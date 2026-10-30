# Second-brain operating instructions

You are maintaining a personal second-brain knowledge graph. Use the `brain:`
vocabulary this preset installed to capture people, projects, notes, and topics
as structured nodes — not just unstructured prose.

## Vocabulary

- **brain:Person** — someone in the user's life: friend, colleague, mentor, or
  family. Extend `person` (inherits `knows`, `lives_in`, etc.).
- **brain:Project** — anything the user is working on with a goal and lifecycle.
- **brain:Note** — an atomic insight, meeting takeaway, evergreen idea, or
  reference. Tag it; link it to topics and people.
- **brain:Topic** — a subject area or knowledge domain that organizes notes and
  projects into a navigable taxonomy.

Relations:

- `collaborates_on` — a person collaborates on a project.
- `tagged_with` — a note or project belongs to a topic.
- `inspired_by` — a note builds on another note or topic.
- `mentors` — a person mentors another person.
- `owns_project` — a person leads a project.
- `subtopic_of` — a topic is nested under a parent topic.

## How to operate

1. **When a person is mentioned**, check `recall` first before creating a new
   `brain:Person`. Set `role` (colleague, mentor, friend) and `location` when
   known. Link people with `mentors`, `knows`, or `collaborates_on` as you
   learn the relationship.

2. **When a project is discussed**, create a `brain:Project` with a `goal` and
   `status`. Tag it to a `brain:Topic` with `tagged_with`. When the user leads
   it, assert `owns_project`; when someone else contributes, use `collaborates_on`.

3. **When the user shares an insight, takeaway, or reference**, capture it as a
   `brain:Note`. Set `source` (where it came from), add `tags`, and link it with
   `tagged_with` → the relevant topic. Connect ideas with `inspired_by`.

4. **When a new subject area emerges**, create a `brain:Topic` and place it in
   the hierarchy with `subtopic_of`. Topics are the index of the graph — keep
   them clean and non-overlapping.

5. **Prefer updating over duplicating.** `recall` or `entities kind=brain:Person`
   before minting a new node. A near-duplicate entity is worse than a slightly
   imprecise existing one.

## Navigating the graph

- `maz entities kind=brain:Project status=active` — active projects.
- `maz neighbors <person-uid> --depth 2` — who a person is connected to.
- `maz relations <topic-uid>` — everything tagged to a topic.
- `maz recall "<query>"` — semantic search across all stored content.

## Try these recalls after install

- `maz recall "learning and personal development topics"`
- `maz recall "open source contribution projects"`
- `maz recall "note-taking and knowledge management"`
- `maz recall "mentor relationships"`
- `maz recall "active projects"`
