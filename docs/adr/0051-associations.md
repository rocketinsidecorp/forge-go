# ADR 0051: Associations and canonical field types

- **Status:** Accepted
- **Date:** 2026-10-27 (owner decisions D22 and D23)
- **Amends:** [ADR 0021](0021-resource-generator.md) (field types, a
  references field, the delete conflict), [ADR 0041](0041-generators-and-destroy.md)
  (`forge destroy` refuses a referenced resource; `join`),
  [ADR 0050](0050-opt-in-authentication-and-organizations.md)
  (`forge generate tenancy` refuses associations)
- **Parity with:** Forge-Rust, planned after Forge-Go reaches 100% (decision
  D37), tracked in [forge-rust#96](https://github.com/rocketinsidecorp/forge-rust/issues/96)

## Context

The generators wrote tables that knew nothing of each other: a comment could
hold a `post_id:uuid`, but nothing checked it, the post's page could not show
its comments, a delete of the post left comments pointing nowhere, and in an
application with organizations nothing stopped a comment of one organization
from naming another's post. The owner asked for Rails' flow, with no code
edited (decision D23):

```sh
forge generate scaffold post title:string body:text
forge generate scaffold comment body:text post:references
forge generate scaffold tag name:string
forge generate join post tag
```

and for one vocabulary of field types (decision D22): `string text integer
boolean timestamp uuid references`, with `int bool time datetime belongs_to`
accepted.

## Decision

1. **Field types.** The canonical names are D22's; the aliases map to them
   everywhere a field is parsed (model, resource, scaffold, `migration`, the
   job payload). Create migrations record the canonical names in their
   `-- forge:generate` header. An unknown type is refused with the list.
   `references` is refused in a job payload (a payload names a record by
   `post_id:uuid`) and in `forge generate migration` (an association belongs
   to the generator of the table that has it, whose code and tests then
   agree).

2. **`post:references` is a belongs_to.** It is the uuid column `post_id`
   (`NOT NULL`; `post:references?` is nullable, which costs nothing: a
   composite key with a NULL member is not checked), the index
   `comments_post_id_idx` and the foreign key `comments_post_id_fk`,
   `ON DELETE RESTRICT`, or `CASCADE` with `--on-delete=cascade`, chosen at
   generation time and recorded in the header. The referenced resource must
   be one a migration records generating, with the same scope; a missing
   parent, a cycle and mixed scopes are refused before anything is written.

3. **Organizations: composite keys, declared by the child.** In an
   org-scoped table the index and the key are `(org_id, post_id)` and
   reference `posts (org_id, id)`. A foreign key needs a unique key on the
   referenced columns, and parents generated before this change only have the
   non-unique `posts_org_id_idx`. Rather than change every parent's create
   migration (which would make every existing parent "edited"), the first
   migration that references `posts` declares
   `CREATE UNIQUE INDEX posts_org_id_id_key ON posts (org_id, id)` and its
   down drops it; later ones find it in `db/migrations` and do not. The
   index costs one more btree on the parent, and makes MySQL 8.4's
   `restrict_fk_on_non_standard_key` and SQLite's parent-key rule hold.
   PostgreSQL's referential checks bypass row-level security, so the key
   holds across organizations whatever the policy says. Together with the
   `TenantContext` predicates of every statement, a cross-organization link
   is refused twice: by the application (the parent's repository does not
   see the row) and by the database. A single-organization application gets
   plain foreign keys.

4. **The service checks first; the key is the backstop.** `CommentService`
   takes the post repository (`NewCommentService(comments, posts, now,
   newID)`) and checks `post_id` before a create or an update that sets it,
   so a missing or foreign post is the field's own error,
   `comment_post_not_found`, 422, even when the select was tampered with. A
   write the key refuses anyway (a concurrent delete) is mapped to the same
   error by the persistence adapter: by constraint name when a resource has
   several references fields, except on SQLite, which does not name it (the
   violation is then `409 reference_violation`). The application layer
   depends only on the parent's port, so the architecture rings hold.

5. **has_many without coupling the parent's code to its children.** The
   child serves the nested lists: `GET /v1/posts/{post_id}/comments` and,
   for a scaffold, `GET /posts/{post_id}/comments`. The parent's page shows
   them through Turbo Frames that load those pages, listed in
   `app/views/posts/_associations.gohtml`, which every scaffold's show page
   includes. The parent's Go code never imports or wires its children; only
   that partial changes. It is generator-owned: its content is derived from
   the migration headers and the children's pages that exist, so generating
   or destroying a child (or a join) rewrites it after checking that it is
   still exactly what the generators wrote, and refuses otherwise, as the
   routes seam does with its checksum. Customization goes into
   `show.gohtml`. The frame's fallback link keeps the list reachable without
   JavaScript. The child's form picks its parent in a select of the newest
   `pagination.MaxLimit` parents of the caller's organization (the row the
   form already names stays selectable).

   What a page shows follows the caller's permissions through the views
   helper `can` (display only; the routes still authorize): the select lists
   the posts only with `posts:read` (otherwise the post id is typed in, and
   no title is shown), a frame appears only with its page's read permission
   (`comments:read`, `tags:read`; Turbo would otherwise show "Content
   missing"), and the join's forms only with `posts:write`. The nested lists
   need the child's read permission alone: `GET /v1/posts/{id}/comments`
   answers 404 for a post the caller's organization does not have and an
   empty page for one without comments, so a member with `comments:read`
   learns whether a post id exists in their own organization. Their
   comments already carry `post_id`, so this tells them nothing their
   permission does not; another organization's posts are always 404.

6. **Deleting a referenced row.** Every resource now maps a foreign key that
   refuses its delete to a typed `ErrPostInUse` (`post_in_use`, 409; the
   pages redirect to the post with an alert), whatever references it,
   hand-written tables included. Each driver reports it differently, and the
   adapters now classify all of them as `sqldb.ErrReferenceViolation`, a new
   exported sentinel: PostgreSQL 23503 and 23001 (`restrict_violation`,
   what `ON DELETE RESTRICT` raises), MySQL and MariaDB 1451 and 1452, SQLite
   `SQLITE_CONSTRAINT_FOREIGNKEY` and `SQLITE_CONSTRAINT_TRIGGER` with the
   foreign key message (how SQLite reports a RESTRICT).

7. **`forge generate join post tag`** is a many-to-many association between
   two resources or scaffolds of one scope: the table `post_tags` with a
   composite primary key (`org_id` first with organizations), foreign keys to
   both sides that cascade (a join row is not data of its own), the reverse
   index, row-level security on PostgreSQL, and `PostTagService` (idempotent
   `Attach`, `Detach`, `Tags`, `TagChoices`). Its routes are guarded by both
   sides' permissions, each route asking the guard twice: `posts:read` and
   `tags:read` to list, `posts:write` and `tags:read` to attach and detach.
   The HTML page (when the first side is a scaffold) is a Turbo Frame with
   forms that stay in the frame. Models are refused: the JSON list reuses the
   second side's response type. `forge destroy join post tag` reverses it;
   the routes seam's block is `// forge:join post_tags`.

8. **Destroy and tenancy.** `forge destroy` of a resource that a references
   field or a join still names is refused, listing them: their foreign keys
   need its table. `forge destroy --migrations` of a child or a join whose
   migration declares a parent's `UNIQUE (org_id, id)` is refused while
   another association to that parent exists: removing the migration would
   leave their keys without it. Destroy those first, or keep the migration
   (destroy without `--migrations`). A join the other way round (`join tag
   post` after `join post tag`) is refused: it would be a second table for
   the same pairs. `forge generate tenancy` refuses an application with
   associations: converting them means replacing every plain foreign key by
   a composite one, and SQLite changes a foreign key only by rebuilding the
   table. Converting before generating associations, or starting with
   `--tenants`, is the supported path; a converting tenancy is future work.

## Consequences

- The owner's flow runs as written on SQLite and MySQL/MariaDB with `--auth`
  and on PostgreSQL with `--tenants` (`--org-scoped` on the resources), and
  the README quickstart test drives it over HTTP, cross-organization refusals
  included.
- Every generated delete and the scaffold's translations gained the in-use
  case, so files generated before this change differ from what the
  generators write now: `forge destroy` and `forge generate tenancy` treat
  them as edited, as they do for any template change.
- A parent's `_associations.gohtml` is one more generator-owned file. An
  application generated before it existed gets it created by the first
  child; its `show.gohtml` must then include it by hand.
- Self-references (a tree of categories) and references to tables a
  hand-written migration created are not generated; they need hand-written
  migrations.
