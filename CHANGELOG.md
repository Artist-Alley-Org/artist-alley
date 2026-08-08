# Changelog

All notable user-facing + wire-format changes to artist-alley.
Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions track the ArchivePub federation spec ([docs/protocol/archivepub.md](docs/protocol/archivepub.md))
where applicable, otherwise note "no-spec-impact."

## [Unreleased]

### Security

- **Changing a post's small cover picture did nothing, and said it worked.** The field was
  documented, accepted, and answered "saved" — while leaving the picture exactly as it was.
  Everything behind it was in place; the one step that actually writes the value had been
  missed (#946).

  It now saves. And, like the main cover picture before it, you can only point it at a file
  you are allowed to open — a file you cannot see answers the same "not found" a made-up one
  does, so it cannot be used to work out which files exist.

- **You could be told you lacked permission when the server simply couldn't tell.** If working
  out what an administrator was allowed to do failed — a momentary database hiccup was
  enough — the answer came back looking exactly like "you are allowed nothing", and the
  admin area told them, in red, that they did not have permission to view the page (#956).

  There was no way to tell that apart from genuinely lacking access: not for the person
  reading it, and not for our own tests, which is why a nightly failure caused by it took
  four rounds of investigation to pin down.

  The server now says which of the two it means. Being unable to determine your permissions
  still shows you nothing you are not entitled to — that part was already right and has not
  changed — but it now reads as a temporary problem to retry rather than an accusation.

- **You could label a post with a studio you have nothing to do with.** Creating a post let you
  name any team on the instance as its owner, and nothing checked whether you were in it. The
  only thing standing in the way was that the team had to exist (#954).

  Nothing became visible that wasn't already — but the label is not just a label. A post
  attached to a studio can be edited and deleted by the people who manage that studio's work.
  So the field quietly handed strangers authority over your post, and put your post in their
  space.

  You can now only attach something to a team you actually belong to, or one you have been
  given the job of managing. A team that exists but isn't yours answers exactly the same as one
  that doesn't exist, so the field can't be used to find out which studios are on an instance.

- **You can put your own upload in a team.** Files could belong to a team — the permission
  rules, the team-only visibility tier and the whole management story were built on it — but
  there was no way to say so when uploading. Only the sample-data tool could do it, which meant
  a demo could show something the product could not actually do (#953).

  Uploading now takes an optional team, under the same rule as posts above. A file in a team can
  be seen by that team when marked team-only, and managed by whoever manages that team's work.

  ⚠️ **This is set when the file is created and cannot be changed afterwards.** Moving a file
  between teams changes both who can edit it and who can see it, so it needs its own thought
  rather than being folded in here.

- **A post's cover picture skipped the check its other pictures got.** Naming a file as a
  post's cover — rather than as one of its contents — was never checked against whether
  you were allowed to open that file. The contents had been checked since the previous
  release; the cover had not, on either creating a post or editing one (#941).

  Nothing was ever shown that shouldn't have been: a viewer who isn't entitled to a file
  still sees a placeholder with its real owner's name. What it allowed was the same
  **unwanted association** the contents check closed — building your post around someone
  else's restricted work without their say-so.

  Both paths now apply the same rule, and a file you can't open answers the same "not
  found" a made-up ID does, so the endpoint can't be used to fish for which files exist.
  Nothing is written when a cover is refused.

### Fixed

- **Renaming a file left the old name showing everywhere else.** Editing a file's title
  or description updated the file — and nothing else. Every post containing it, and every
  IIIF manifest describing it, went on serving the old text until the server happened to
  restart (#935).

  This is the same staleness that was fixed for deleting and restoring a file in the
  previous release, on the path people actually use every day. It survived because
  attention went to the dramatic operations: deletion looks like it should invalidate
  things, an ordinary edit doesn't.

  Permanently deleting a file had a related gap. Because the database removes a file's
  subtitle tracks and post memberships automatically, that cleanup happened in parts of
  the system that never ran any code — so they kept answering from before the deletion.
  Those caches are now told explicitly.

- **A public collection handed out its guest list.** Listing the access grants on a
  collection passed for the owner, for an administrator — and for **anyone at all with
  an account**, as long as the collection was `public`. Every grant row came back: who
  the collection had been shared with, at what permission level, by whom, and when it
  expires (#933).

  Marking a collection `public` is a statement about what is *in* it. It is not a
  statement about **who the owner individually shared it with** — that is information
  about the owner's working relationships, and it was reaching people with no connection
  to the collection whatsoever. Posts settled this same question a while back; the
  collection surface never got the same treatment.

  Listing a collection's grants now requires **write** access — owner,
  `collections.admin`, or `system.admin`. Someone who holds only a read grant can still
  use the collection; they no longer learn who else was let in. The two surfaces now
  apply the same rule.

- **You could put someone else's restricted work in your post.** Creating a post, or
  attaching a file to an existing one, checked only that the file *existed* — never that
  you were allowed to see it. Any signed-in account could name any file on the instance
  as part of its own post (#922).

  This never exposed the file itself: viewers who are not independently entitled see a
  placeholder carrying the real owner's name, exactly as before. What it allowed is
  **unwanted association** — attaching an artist's restricted work to your post without
  their consent, so that everyone who *is* entitled to see it meets it framed by you.

  Both paths now apply the same rule the collection surface already applied: you may
  attach a file you can actually read. A file you cannot read answers the same "not
  found" a made-up ID does, so the endpoint cannot be used to probe which IDs are real.
  Nothing is written when a member is refused.

- **Anyone signed in could edit or delete anyone's assets.** `PATCH /assets/{id}` and
  `DELETE /assets/{id}` checked one thing: that you were logged in. Not that you owned
  the asset, not that you had any standing over it — just that you had an account. Any
  account could therefore retitle, re-tag, rewrite the metadata of, or soft-delete
  **every asset on the instance**. Posts have gated on `posts.admin` and collections on
  `collections.admin` since they were written; assets were the outlier (#930).

  The damage was lopsided. Deleting took an account. **Undoing** took `system.admin` —
  so one ordinary user could remove a studio's entire library and nobody below a
  super-administrator could put it back.

  Both endpoints now answer **403**, and the row is untouched. You may edit or delete an
  asset if you own it, if you hold the new `assets.admin` capability, or if you are a
  `system.admin`.

- **`assets.admin` — manage a team's files without owning them.** A new capability for
  the case the owner asked for: *"a concept art director should be able to manage a file
  of someone on their team"*, while *"members shouldn't be able to change other
  member['s work]"*. Grant it **scoped to a team** and it covers that team **and every
  team beneath it** — a grant on a division reaches its squads without granting anything
  outside the division. Grant it globally and it covers the instance.

  It does **not** confer publication. Changing an asset's `status` is what decides
  whether a stranger can see the asset at all, so it is a decision about disclosure
  rather than about content, and it stays with the owner and `system.admin`. A team lead
  can fix your title; they cannot push your unfinished work live. The same line is drawn
  for posts: a *team-scoped* `posts.admin` can now manage its team's posts but can
  neither change a post's `visibility` nor **grant anyone access to it** — both are the
  same lever reached through different endpoints. (A global `posts.admin` is the instance
  moderator role and is unchanged.)

  Sharing a team with someone still grants you nothing over their files. Only the
  capability does.

- **Team-scoped moderators could not moderate.** `posts.admin` was only ever consulted as
  a *global* grant, so an art director whose grant was scoped to one team could not touch
  that team's posts. It is now scope-aware, the same way `assets.admin` is.

- **`posts.admin` and `collections.admin` were impossible to grant.** Both existed only
  as strings inside the server. Neither was ever a row in the capabilities table, and
  every grant path is foreign-keyed to that table — so granting either one, to a person
  or to a role, failed outright. The two moderator gates that read them could only ever
  be satisfied by a full `system.admin`. Both are now real capabilities you can grant.
  Doing so is still a deliberate act: neither is attached to any role, and seeding a
  capability gives nobody anything until an administrator hands it out.

### Added

- **Publishing can be handed to someone other than the owner.** Making a file live, retiring
  it, or bringing it back were reserved to whoever uploaded it and to system administrators.
  A team lead trusted to manage a library could edit and delete files, and could not publish
  one (#938).

  Three separate permissions now exist, and each covers only the moves it names — publish,
  archive, un-archive. Someone given the power to retire work cannot use it to make work
  public, which matters because making a file **live** is what makes it visible to people
  who are not signed in. That one act always requires the publish permission, by whichever
  route it is reached.

  The two halves of managing a file are also separated now. Being trusted to publish does
  not carry the power to rewrite a title, and being trusted to edit does not carry the power
  to publish. Previously the two came bundled, which is why neither could be delegated on
  its own.

  ⚠️ **Currently this only works for permissions granted across the whole instance, not for
  ones scoped to a single team** — nothing yet assigns a file to a team, so a team-scoped
  grant has nothing to match against. Being fixed (#953).

- **Someone who manages your team's files can now read their details — but still can't
  open them.** A team lead with permission to edit, delete and restore their team's work
  was, until now, shown the same blank placeholder as a stranger. They could rename a file
  they had never been allowed to see, and delete one they had never been shown (#939).

  They now see the **written details** — title, description, tags, the rest of the
  metadata — for exactly the files they are entitled to manage. They still cannot see the
  **picture**, not even the blurred preview, and still cannot download the original. The
  result is a fuller placeholder rather than an open door.

  This deliberately does not turn a management permission into a viewing permission. Those
  remain separate: being trusted to tidy up a library is not the same as being cleared to
  look at everything in it, and a great many studios need exactly that distinction for
  work under embargo or licensed from someone else.

  A permission granted on a parent team reaches the teams beneath it, as it already did
  everywhere else.

- **You can undo your own delete.** Assets, posts and collections now record **who**
  deleted them, and restoring is no longer administrators-only: if you deleted it, you
  can put it back. If someone else deleted it, you cannot — you ask for it back instead,
  which is the case the owner described as *"users should be able to recover their own
  deleted files, unless deleted by an admin. Then they would need to request for
  restoration"* (#931). The request-and-approve flow for that second case is not built
  yet; #931 stays open for it.

  Whoever could delete a thing can now reverse themselves, including a team lead acting
  under a scoped `assets.admin`. Anything deleted before this release, and anything
  removed by the automatic retention sweeper, has no recorded deleter and remains
  restorable by a `system.admin` only.

- **An asset you cannot open no longer hands you its metadata.** A `restricted` asset
  owned by someone else refused you its **bytes** — `/file`, `/download` and every
  `/variants/*` returned 404, correctly, and always had. It then described itself in full
  through every surface that lists it. `GET /assets/{id}` returned **200** carrying the
  title, the description, the complete SHA-256, the exact byte size, the original
  filename and the whole free-form `metadata` blob. Browse returned the same. Search
  returned the title and description; the autocomplete would **complete that title from a
  prefix**, letter by letter; and the sensitivity facet counted the asset as
  `restricted 1`. The card on screen said "restricted" the whole time — the API behind it
  did not (#899).

  The hash was the sharpest of these. It is a content identifier, so it confirms whether
  a file you already hold is the same one, and it would have survived any later
  tightening of the other fields.

  An asset you may not open now returns a **placeholder** carrying exactly three things:
  its id, a `restricted` marker, and **the owner's display name**. Nothing else — not the
  title, not the file type, not the dimensions, not the thumbnail blur. Fields are
  **absent** rather than blanked, so you cannot tell "withheld" from "genuinely empty" and
  infer from the difference. The same shape now comes back from the single asset, the
  browse list, a search hit, the similar-assets panel and a post or collection member —
  one rule, `visibility.FieldsReadable`, in one place, rather than a version of it per
  surface. Autocomplete is the one exception, and it drops the row instead: a completion
  *is* the title, so there is nothing to withhold.

  **The row is still there**, which is the half worth stating. Sensitivity gates content,
  not rows (ADR 0064), so a restricted asset stays in your feed and in your search
  results as a placeholder with its owner's name on it. That is deliberate: you are meant
  to be able to tell there is something there you cannot see, or "request access" has
  nothing to point at.

  **Nothing changes for people who can already open the asset.** Its owner sees their own
  work in full at every tier, including drafts; so do administrators and the read-all
  content role the public demo runs on. The facet counts moved with the same rule — they
  now count what *you* can open, so you still see `restricted 3` for your own work and no
  longer see a count of other people's.

  The viewer also stops offering a **Download original** button for an asset whose bytes
  it knows it cannot fetch. The download always 404'd; now it is not drawn.

  No-spec-impact for federation — asset metadata was never sent to peers. **Wire-format
  change:** the `Asset` schema's `required` list shrank to `id` and `restricted`, because
  a contract that demands `title` and `file_hash` cannot express a payload that withholds
  them. Every field a readable asset carried, it still carries.

- **You can only put something in a collection if you can actually see it.** Adding an
  asset to a collection was authorised against the **collection** — "is this your
  collection" — and never looked at the asset at all. Anyone who could create a
  collection could therefore pin any asset on the instance, including one they had never
  been allowed to view, given nothing but its UUID (#882).

  Adding now requires the asset to be **readable by you**: it has to exist, not be in the
  trash, and you have to be entitled to its content under its sensitivity tier — the same
  standard that decides whether a collection member renders for you at all, rather than a
  second, slightly different rule that could drift from it. Your own work is unaffected at
  every tier, including drafts.

  **Collecting other people's work still works**, which is the half worth stating: if you
  can view it, you can collect it. That is the whole point of the feature, and this only
  removes the cases where you could collect something you could never open.

  It also closes a probe. An asset you cannot read and a UUID that does not exist now
  produce the **identical** response — same status, same body — so the endpoint can no
  longer be used to confirm that a guessed asset id is real. Nothing was exposed by the
  old behaviour: a member you cannot read has rendered as a placeholder carrying only the
  owner's name since #883. What it leaked was **existence**, and what it broke was the
  integrity of the collection itself.

  No-spec-impact. Removing from a collection is deliberately unchanged and still needs no
  readability check: it only un-pins a row from a collection you already own, and gating
  it would strand a member whose sensitivity was raised after you collected it.

- **Sharing a collection with another instance now shares only the members you own.**
  A federated share on a collection granted the peer scope over **every** asset in it,
  and the only ownership check in the system sat on the container: you may share a
  collection because you own the collection. Nothing then re-checked the contents. The
  membership lookup that walks collection → asset carried no constraint on who owns the
  member at all (#893).

  Put someone else's asset in your own collection — which the API already allows, since
  adding to a collection is authorised against the collection and never against the
  asset — share the collection with a peer, and the peer was granted scope over an asset
  that was never yours to share. This matters more than the equivalent mistake against a
  local reader: a peer is a **separate instance**, which takes its own copy of that
  decision and can act on it afterwards — there is no single place to take it back.

  A container share now confers scope on a member only if the share's grantor could have
  shared that member **directly** — they own it, or they hold `system.admin`. That is the
  same pair of conditions the grant endpoint already enforces, and it is now asked from
  one place rather than two, so the two answers cannot drift. An asset with no local
  owner — a federated mirror, a system import — is nobody's to re-share and is refused.

  **Sharing a collection of your own work is unchanged**, which is the half worth
  stating: the fix is per member, not per collection, so a shared collection still
  carries every member its grantor owns, and an admin's share still reaches everything.
  A refusal on this ground carries its own reason — `grantor_not_owner` rather than the
  misleading "no share row" — so an operator reading a rejection can tell "the grant was
  never the grantor's to make" from "there is no grant".

  Nothing was exposed in a shipped release: the decision function this fixes has no
  caller yet — the inbox dispatcher does not consult it, so no inbound activity has ever
  been admitted or refused by it. The guard lands **before** that wiring and before the
  change that lets a collection hold someone else's work (#882), so the window in which
  the hole would have been reachable never opens. No-spec-impact — no wire format
  changes, and no existing share row is revoked or altered.

- **Three known CVEs stop shipping inside the published image, and the blind spot that
  let them sit there is closed.** `ip-address` — one **high**, two medium — reached us
  through puppeteer's proxy-resolution chain in the headless three.js preview renderer.
  That renderer is not developer tooling: both the release Dockerfile and the
  local-compose one install `scripts/threejs/package-lock.json` and copy the resulting
  `node_modules` into the runtime stage, so the vulnerable code was in the artifact an
  operator would actually run. It is now at 10.4.0, a plain transitive bump — the two
  direct dependencies, `puppeteer` and `three`, are untouched, and no `overrides` block
  was needed, because the range `socks` already declares admits the patched version.

  **The reason these sat open is the part worth fixing.** Dependabot raises security
  *alerts* for any lockfile in the repository, but it only opens a *fix* PR for
  directories listed in its config — and that config watched four directories, none of
  them this one. Nothing was ever going to bump it. `scripts/threejs`,
  `scripts/dogfood/ui` and `seed/scripts` each have their own `package.json` and
  lockfile and are now watched on the same weekly cadence as `web/`; the two
  `infra/docker/` images are now watched alongside the root Dockerfile, which the docker
  updater never covered because it does not recurse into subdirectories. Without that
  second half the next advisory would have waited exactly as long as these did.

  No behaviour change: the renderer's own smoke test — chromium launching and rendering
  all ten model formats, which is the code path the proxy chain sits in — passes against
  a locally built production image. No-spec-impact (#905).

### Changed

- **A search no longer answers with a shelf of things you did not ask about.** The
  **Featured** rail — the curated strip of collections an operator pins to the hub — sat
  at the top of the browse page whether or not the page was still browse. Typing into the
  navbar search box takes you to the same route with a `?q=`, so your results arrived
  underneath a row of curated collections that had nothing to do with what you typed, and
  the first thing on screen after a search was the thing you were looking at before it
  (#908).

  The rail now renders on **unfiltered browse only**. Search results are just the results.

  Nothing else changes about it. An unfiltered browse still opens on the rail for
  everyone, signed in or not — for a signed-out visitor it is the entire landing page
  (posts are members-only), which is the case the rail exists for and the one worth being
  careful about (ADR 0065, #417). Changing view mode, sort direction or the feed pill
  keeps it, because those rearrange the same set of posts rather than asking a question.

- **Creating a collection no longer asks you a question it already knows the answer to.**
  The New collection dialog offered four visibility buttons — Private, Org-only,
  Followers, Explicit share — pre-selected to **Private**, which is precisely what the
  server picks when you say nothing. It was a required-looking decision, in front of a
  collection that did not exist yet, whose default was already the safe answer and which
  is a click away from being changed afterwards from **Edit details** (#914).

  The dialog now asks for a name and a description. It says what you get — collections
  start private — instead of asking you to choose it.

- **Searching no longer feels like leaving the app.** Everywhere else in artist-alley,
  work is a wall of tiles: the artwork itself, at the size you chose, with the hover
  preview and the view mode you were last using. **/search** was the exception. It
  returned a column of text rows — a title, a line of description, and `score 1.000`
  beside each one — in a narrow column with a fixed filter rail down the left. The same
  piece of art you had been looking at as a tile a second earlier came back as a line of
  type. Search results are now the SAME grid, the SAME cards, and the SAME view modes as
  browse: grid, masonry, feed, thumbnail. Switch the home feed to masonry and your
  searches are masonry (#850).

  That was not a styling change. A search hit only ever carried a title, a summary and a
  blur-up thumbnail — nothing a tile could be drawn from — which is why the page rendered
  text in the first place. A hit now carries what a card needs: the file type (so the
  video and 3D badges appear and the hover scrub plays), the responsive image rungs, the
  recorded dimensions that let a masonry tile reserve its shape before the image loads,
  and — for a post — its cover art, its like and comment counts and how many pieces it
  bundles. A collection hit carries its visibility so the tile badges it. **None of that
  reaches a caller who cannot open the asset**: a restricted result is still a
  placeholder carrying its id, the marker and the owner's name, and nothing else. The
  widening went *through* the same permission check the rest of the app uses, not around
  it.

  Three more things changed with it:

  **The filter rail is gone.** It was a fixed 16rem column that could not fit beside a
  grid on a phone, so /search scrolled sideways at 390px (#901). Facet counts now open in
  a panel — the same panel at every width, so there is nothing to retrofit for small
  screens later — and the kind filter (**Everything / Artwork / Posts / Collections**)
  sits as chips over the results, where it filters for real and stays in the URL so a
  filtered result page is a link you can send someone. The facet counts themselves are
  counts, not controls: the search API accepts no facet filters yet, and the checkboxes
  that used to sit beside those numbers never filtered anything.

  **The advanced query builder is a panel, not a page.** It used to be its own
  destination at `/search/advanced`, which made "advanced" a separate *mode* of
  searching — you left your results, built a query somewhere else, and arrived back at a
  different page. It now opens over the results you are already looking at and composes
  the same query. Reverse-image search moved with it. `/search?advanced=1` opens it
  directly.

  The button beside the navbar search box changed with it. It read **Advanced search**
  and it has always gone to `/search` — so the label named a page that no longer exists,
  while the place it opens is now simply where you search. It reads **Search**, and it
  **carries whatever you have typed in the box** rather than dropping it: a control named
  after a search box next to it, that navigated away and lost your query, would be a
  trap.

  **The relevance score is no longer printed on every result.** An artist does not need
  to be told that their own drawing scored 1.000; the ordering it describes is the
  ordering on screen. Thumbs-up / thumbs-down feedback is still there, on hover over the
  tile.

  Results also use the whole window now instead of a ~1150px column, which on a wide
  display is the difference between five tiles and eleven.

  No-spec-impact for federation. **Wire-format change:** a `/search` hit's `extra` object
  gained per-type presentation fields, and its `thumbhash_b64` key is now spelled
  `thumbhash` — the name every other endpoint uses for the same value.

### Added

- **The feed no longer shows you doors you cannot open.** Since #899 and #883, a piece of
  work you are not entitled to see comes back as a **placeholder** — a tile that names its
  owner and says, in effect, "there is something here, and it is not for you". It is honest
  about what exists, and it is what #913's **Request access** button has to sit on. But on
  an instance where a lot of work is restricted, a feed can be mostly placeholders, and
  that is a wall of locked doors rather than a gallery. On our own seed data one account's
  feed was 82 posts of which **27 were entirely placeholders** — a third of the grid.

  So the browse feed **leaves them out by default** (#891 built it, #921 made it the
  default). The line we settled on, which is why the other screens behave differently: a
  placeholder belongs where you **asked a question** or **opened a container** — not where
  you were handed a feed.

  Three things happen in the feed, and the third is the one that took the thought:

  - A restricted item is **left out** of a post rather than drawn as a placeholder.
  - A post whose items are **all** restricted drops out of the feed entirely. The
    alternative — an empty card where a post used to be — is worse than the placeholder it
    replaced.
  - **Your own posts never disappear.** A post you wrote can contain someone else's
    restricted work, so the rule above, applied literally, would delete your own post from
    your own feed. It does not. Your post stays, with its restricted items hidden like
    anyone else's.

  **Where the placeholders still are, unchanged:** **open** a post and you see them, and
  can still ask for access. Look inside a **collection** and you see them there too — so
  you can tell there is restricted work in a project without it flooding your feed. Neither
  of those is an oversight. The reason an all-restricted post leaves the feed is that an
  empty card is worse than a placeholder, and hiding the items on the post page itself
  would put that empty card back on the one screen the rule could not reach.

  It cannot show you anything you could not already see. The rule about what you may read
  runs first and is completely untouched by this; the feed only decides how much of its own
  answer to draw.

  **Want them back?** **Settings → Preferences → Feed filters → Show items I don't have
  access to** restores the old feed exactly, placeholders and all. The trade is stated in
  the setting's own help text rather than left to be found: **Request access** lives on the
  placeholder tile, so while that setting is off the button is not in your feed — open the
  post, or a collection it is in, to ask.

  The setting travels with your account, so it is the same on every device you sign in
  from, and it applies from the first frame of the page rather than after a flicker.

- **You can now share a post with someone.** Three rounds of work built the whole
  receiving half of post sharing — an ACL row on a post grants read (#667), the person you
  share with gets a notification and the post lands on their "Shared with me" (#875), and
  they cannot enumerate the rest of the guest list (#876). All of it worked, and there was
  no way to create a grant. `GET/POST/DELETE /posts/{id}/acls` had **zero callers in the
  frontend**; "Share" on a card copied a link (#880).

  Posts now have **Manage access…**, on the post's own ⋮ menu and on the ⋮ of a post card
  you authored. It is the same dialog collections have always had, generalised rather than
  copied — one share surface for both, so the next change lands in one place.

  Three things the dialog does that the collection-only version did not:

  - **You type a username, not a database id.** The field used to be free text
    placeholdered "id or username", and a username typed into it wrote a row that granted
    nothing and notified nobody — the grant is keyed on the numeric user ref, which the
    typed name never matched. The name is now resolved before the grant is written: a
    typo is an error on screen, not a dead row nobody sees. The list of current grants
    reads back as names too, so you can tell who you shared with.
  - **A share can expire.** Never / 1 hour / 24 hours / 7 days / 30 days, or a date you
    pick. An expired grant stops granting on its own — the read rule checks it, so there
    is nothing to come back and revoke — and the post drops off the other person's
    "Shared with me" when it lapses. Expiry was always settable through the API and never
    offered in the app.
  - **It only offers grants that work.** The picker used to offer *user*, *role* and
    *team*. Only *user* confers anything: role and team are ADR 0010 Layer 5 and are
    unimplemented on both the post and the collection read rule, so those two options
    recorded a row that looked like access and was not. The dialog now grants to users
    only. Any role or team row already in a list is still shown, marked as granting
    nothing yet, rather than quietly hidden.

  Unchanged: who may grant (the author, or `posts.admin` / `system.admin`), what a grant
  confers, and who may see the guest list. This is an entry point onto the existing rules,
  not a new one.

- **A restricted item that says "no" now also says "you can ask".** A restricted asset you
  cannot open renders as a placeholder carrying its owner's name and nothing else (#883,
  #899). That was the whole of it: the tile stated a refusal and offered no way past it,
  and the request workflow behind it — a full typed lifecycle with a requester list, an
  approver queue and a decision dialog — had **no entry point anywhere in the app**.
  Nothing in the frontend had ever called `POST /assets/{id}/request-access` (#881).

  The placeholder now carries a **Request access** button, on the grid tile and on the
  restricted member inside a post. It opens a short dialog — an optional line about why
  you are asking — and files the request. Signed-in viewers only: an anonymous visitor
  has no account to ask from, so they get the plate as before rather than a button that
  cannot work.

  **The placeholder still leaks nothing.** The button's label, its aria-label and every
  word of the dialog are fixed strings plus the owner's display name. The asset's id is
  posted and never rendered. The rule is the owner's — *"the placeholder should never
  leak info. Not even title. Only the owner's name."* — and it is now held by a test that
  takes every string the tile puts in the DOM, attributes included, and requires each one
  to be on an allow-list, so a field added later fails by default instead of shipping.

- **Someone is told when a request arrives, and the artist can answer it themselves.**
  Two gaps made "request access" a message into a void even once it could be sent.

  **Nobody was notified.** The only notification in the request lifecycle fired on the
  *decision*, to the requester. Creating a request pinged no one — the approver queue
  filled in silence and `/admin/requests` was a page you had to think to visit. A new
  request now notifies the asset's **owner** and every approver, through the existing
  notification pipeline, so it inherits your channel preferences and block settings. The
  notification carries ids and nothing else: no title, no filename, not even the reason
  you wrote.

  **The owner could not decide it.** Deciding required `share.grant` or `system.admin` —
  operator capabilities an artist has no reason to hold — so the person with the
  strongest claim to answer a request about their own work was the one person who
  couldn't, and every request routed through an administrator who knows nothing about
  the piece. An asset's owner can now grant or deny requests on their own assets, from a
  new **Requests for your work** section on **/account/requests**, holding no capability
  at all. The section appears only when there is something to decide.

  That widening is deliberately narrow. A request names a capability, and that name is
  chosen by the *requester* — so an owner who could decide any request could be talked
  into granting `system.admin` from a panel that looks like it is about a picture. An
  owner can therefore decide only requests naming `content.access.request`, the code the
  Request access button submits, which grants nothing on its own. Everything else still
  needs a real approver.

  **Asking twice is not asking twice.** Repeating a request you already have pending
  returns the one you already sent, rather than filing a duplicate the approver would
  have to deny. A request that was **denied** does not block a new one: a refusal is
  final for that request, not for you.

  **What approval does not do — and we say so in the app.** Granting a request records
  that the owner agreed. It does **not** currently reveal the asset, because there is no
  way yet to say "this one person may view this one asset" — capability grants have no
  per-asset scope, and the only capability that opens restricted content opens *all* of
  it. That is a known deferral (ADR 0064), tracked as #912. Both the request dialog and
  the decision panel say it in as many words, because a granted request that silently
  changes nothing would be worse than no button at all.

- **Four more tiles on your account page now lead somewhere.** The grid at
  **/account** has always drawn every tile it knows about, whether or not the page
  behind it existed, so a good number of them were a click into a "coming in a later
  phase" panel. Four of those are now real (#600):

  **Account → Following** lists the people you follow and the people who follow you, on
  two tabs, with the date each connection started and an *Unfollow* button on your own
  list. Clicking anyone opens their profile. There is no *remove a follower* button:
  blocking is how you sever an incoming connection, and that lives on the profile page.
  Read-only over endpoints that already existed — `GET /users/{ref}/following` and
  `GET /users/{ref}/followers`, plus `DELETE /users/{ref}/follow` for the button.

  **Account → Keyboard shortcuts** is the cheatsheet, and this is the first time an
  ordinary signed-in user can reach one: the existing copy sits under **/admin/help**,
  which shows a "no permission" panel to anyone without an admin capability. It also
  got considerably longer, because the old list only covered video playback and the
  search box. It now documents the viewer's navigation keys, the ebook reader, sprite
  sheets, and the whole whiteboard — tools, clipboard, and zoom — grouped by where each
  key works, with the caveats spelled out (there is no global shortcut; arrow keys mean
  two things at once on a video in a feed; the whiteboard's F wins over the viewer's).
  Every row was checked against the handler that implements it, and rows for keys we
  never bound were dropped, including the old *Esc = exit fullscreen* line. Operators
  see the same list at **/admin/help/shortcuts** — it is one catalogue rendered twice.

  **Account → Help & support** points at the documentation site, the cheatsheet above,
  and the project's issue tracker. It deliberately does not mirror the /admin/help
  section, because every link there needs an admin capability to open.

  **Account → Access requests** is not new — the page has existed since 1.17.E — but it
  had no entry in the account menu, so nothing anywhere in the app linked to it. You
  could only reach it by typing the URL. It now sits next to *Shared with me*, which is
  its natural pair: one is access someone gave you, the other is access you asked for.
  Requesting access from an asset you cannot open is still a separate piece of work
  (#881).

  Still placeholders, because each needs a backend that does not exist yet: bookmarks,
  drafts, trash, activity log, stats, subscriptions, connected accounts, and AI
  preferences. No-spec-impact.

- **A post shared with you now tells you, and stays somewhere you can find it.**
  Since #667 a share genuinely grants read, but nothing announced it and nothing
  collected it: no notification was sent, and the browse grid shows the walled-garden
  `org-only` tier whatever you have been granted, so a shared post never appeared in
  it. Sharing only worked if the sharer separately sent you a link (#875).

  Two things change. Granting a person read on a post now sends them a notification —
  *A post was shared with you* — naming the post and linking straight to it, delivered
  through the same channel preferences and block rules as every other notification, so
  you can mute it in Account → Preferences like any other event. And **Account → Shared
  with me** is a new page listing every post someone has given you access to. Access
  that lapses or is revoked drops off that page immediately; there is nothing to tidy
  up. Grants to a `role` or `team` name no single recipient and notify nobody, matching
  the fact that they do not grant read yet either.

  The browse feed is deliberately unchanged. Shares are few and important, which makes
  them worth announcing rather than burying in the busiest grid in the app — every
  comparable tool reaches the same conclusion. New endpoint
  `GET /account/shared-posts` returns the same `PostList` shape as the feed; new
  notification verb `post_shared_with_me`. (Finding a shared post by *search* was a
  separate rule that did not honour grants; that is fixed below, in the same release,
  by #873.) No-spec-impact.

### Fixed

- **Deleting an asset now removes it from the posts it was in.** Deleting an asset
  reported success, and the asset really was deleted — its bytes stopped being served and
  it left the browse grid. But every **post** that included it went on showing it: the
  title, the description, the full SHA-256, the byte size, the dimensions, the metadata.
  Not a stale thumbnail — the whole record, served fresh on every request, to everyone,
  indefinitely. Restarting the server was the only thing that cleared it (#920).

  The database was right the entire time; the query that lists a post's contents has
  always skipped deleted assets. What was wrong is that deleting an asset never told the
  posts holding it that their cached copy was now wrong, because deleting an asset does
  not touch any post. Now it does, and so does **restoring** one — restore had the mirror
  image of the same fault, where an asset you brought back stayed missing from its posts
  until a restart.

  Worth stating plainly: for as long as a post outlived the asset in it, "delete" did not
  mean deleted anywhere someone was looking at that post. No-spec-impact; no wire-format
  change.

- **Sharing something with a username no longer silently does nothing.** `POST
  /posts/{id}/acls` and the collection equivalent take a `principal_id`, and that field
  wants a numeric user **reference**, not a name. Passing a username — the obvious thing
  to pass — was accepted, stored, and answered **204 No Content**, exactly as a successful
  share does. Nothing was shared. The row could never match anyone, the person was never
  notified, and there was no way to tell from the outside: the grant appeared in the
  access list looking real (#916).

  The API already knew. The notification step parses the same value, and when it failed to
  it wrote a line to the server log and returned — after the useless row had been written.
  It now rejects the request with **400** and says what the field wants, and writes
  nothing.

  The same check covers asset-type ACLs, where a role or team id must be a UUID.

  **`role` and `team` grants on a post or collection are now refused** rather than stored.
  They were in the same position as a username: recorded, and matched by nothing, because
  group-based access to content is not built yet. They now return 400 saying so. Grant to
  individual users instead. (Asset-type ACLs are unaffected — role and team work properly
  there and continue to.)

- **A post with nothing attached to it now opens.** Not "renders badly" — did not open at
  all. It showed a loading shimmer, forever, on a blank screen: no title, no description,
  no comments, no author, and for its own author no **Edit post**, no **Delete post** and
  no **Manage access**. Behind that shimmer the page was re-requesting the post as fast as
  the network would carry it — **over 1,600 requests in six seconds**, measured, for as
  long as the tab stayed open (#918).

  Two faults stacked, and it took both to hide either. The post loader skipped a re-fetch
  when it was already showing the post you asked for **and had at least one item to show**;
  for a post with no items that second half was never true, so it re-fetched, which
  re-rendered, which re-fetched. And every scrap of post detail — the header, the author,
  the ⋮ menu — is contributed by the viewer's details panel, which the shell only mounts
  when there is an item to view, so the empty state was a single grey sentence and nothing
  else.

  A post reaches this state without anyone doing anything strange: its last attachment gets
  deleted, or it never had one (a text post, ADR 0073). It now loads once and renders the
  post — description, likes, comments, and the author's own full menu — beside a plain "no
  assets in this playlist".

- **The ⋮ menu on a post no longer keys off how many items the post has.** The whole menu
  was drawn only when there was at least one visible item. The guard was about the
  **items**; the menu is about the **post**. The actions that operate on the contents (add
  all to a collection, download all, tag all) are still hidden when there are no contents.
  Everything that operates on the post — edit, delete, **Manage access** — stays (#918).

- **Share is no longer offered on a collection that is not yours.** The action toolbar's
  owner-only block closed one button early, so **Share** was drawn for every reader.
  Clicking it opened the dialog, and the server — correctly — refused with *"not the
  collection owner"*, which since #915 is now visible rather than silent. Nothing leaked;
  it was simply an offer that could not be accepted, and the refusal was the first anyone
  heard of it. Share now follows the same ownership rule as Edit and Upload here. **Copy
  link** is unchanged for everyone: if you can open a collection, you can link to it
  (#918).

- **A post you can read is now a post you can find.** Search asked a narrower question
  than the feed did. Browse, `GET /posts/{id}` and the post-by-asset lookup all applied
  the real rule — your own posts at every tier, public and **org-only**, `followers`
  posts by people you follow, `private` posts if you moderate, and anything explicitly
  **shared with you** — while `/search`, the search **facets** and the search-box
  **autocomplete** applied only "public, or written by me". Everything else was dropped
  from your results. No error, no message, no empty state that explained itself; the
  post was on your feed and simply did not exist as far as search was concerned (#873).

  `org-only` is the default tier for a post, so in practice most of the corpus was
  unfindable. Typing a post's exact title returned nothing. The tag counts beside your
  results were computed the same way, so a tag that appears only on org-only posts read
  `0` or went missing entirely — and an undercount looks exactly like a correct count.
  Autocomplete would not complete a title it had no business hiding.

  All four surfaces now compose **one expression of the rule**, in one place, so they
  can no longer answer differently. **This widens what search returns.** It returns more
  posts than it did — specifically, the posts you could already open from your feed and
  from their own page. It does **not** widen who may read a post: a post becoming
  findable does not make its restricted members' fields readable, an expired share still
  grants nothing, and administrators' trash view still applies the same authorization it
  did before. Nothing became readable that was not readable yesterday; things you could
  read became reachable through the search box. No-spec-impact for federation.

- **The app container's memory ceiling was too low, and it was killing itself.** On
  our own CI host the app process was OOM-killed by the kernel roughly once every 90
  minutes — eleven times in sixteen hours — always against the container's *own*
  ceiling, never because the machine was short of memory (that host had 40 GiB free
  throughout). A process that dies mid-render leaves connection errors, half-written
  data and timeouts behind it, so this was showing up as a scatter of unrelated-looking
  failures rather than as one problem.

  **The ceiling changed, so operators who did not set it get a new default:**
  `AA_APP_MEM_LIMIT` is now **8g**, up from 4g. Measuring a ~150-asset preview render
  storm put the peak at **5.4 GB** — which the old 4 GB default cannot hold at all, so
  any instance that renders previews was relying on the kernel's mercy. `mem_limit` is
  a ceiling and not a reservation, so a machine that never reaches it gives up nothing.
  If you pinned `AA_APP_MEM_LIMIT` yourself, your value still wins, and 4g is now known
  to be too small for preview work.

  **`AA_GOMEMLIMIT_RATIO` has been 0.9 and is now 0.8** — the share of that ceiling
  handed to the Go runtime. The reserve is not spare change: preview rendering shells
  out to ffmpeg, ghostscript, ImageMagick and a headless-browser 3D renderer, all of
  which live inside the same container and were measured holding a full **1.0 GB** at
  peak, none of it visible to the Go runtime's own accounting. 10 % of the ceiling
  never covered that.

  **And `AA_GOMEMLIMIT_RATIO` now actually does something in Docker.** It was
  documented as a knob but was never passed into the app container, so setting it had
  no effect on any containerised deployment — the process never saw the variable. It is
  forwarded now (#887).

  The peak itself is untouched by any of this: 3.3 GB of it is live, in-flight render
  buffers that no garbage collector can shrink. Bounding *that* is a separate piece of
  work. No-spec-impact.

- **Two account tiles were filed as unbuilt long after their pages shipped.** *Saved
  searches* and *Messages* were still marked "not built yet" in the account menu's
  registry even though both pages had been working for releases. **Nothing on screen
  changes** — the tile grid never filtered on that flag, so both tiles were already
  visible and already opened their real pages. This is bookkeeping, not a feature, and
  it is listed only because the flag now has teeth: a tile claiming to have a page is
  checked against the actual route tree on every test run, so the next tile that
  ships — or that is marked ready a release early — fails the build instead of waiting
  for someone to click it and get a 404. Three tiles carried a wrong flag for a full
  release with no signal at all (#600). The `mscrnt/artist-alley` GitHub links on the
  admin help pages, left over from the move to the `Artist-Alley-Org` organisation, now
  point at the current URLs. No-spec-impact.

- **Sharing a post no longer shows the recipient the rest of the guest list.** Wiring
  grants into the read rule (#667) had a side effect nobody wanted: `GET
  /posts/{id}/acls` let anyone who could read a post list its grants, so sharing a post
  with one person disclosed to them everybody else it was shared with, who granted each
  one, and when each expires (#876). Any signed-in user could do the same for any
  `org-only` post.

  Listing a post's grants now requires the ability to edit the post — its author, or an
  administrator. Who a post is shared with is management information about the post,
  not part of its content, which is the line collections have always drawn. Nothing
  about reading a shared post changes: the share still works, the post still opens, and
  it still appears on your *Shared with me* page. No-spec-impact.

- **Sharing a post with someone now actually shares it.** Granting a person read on
  one of your posts (`POST /posts/{id}/acls`) recorded the grant, listed it back, and
  changed nothing whatsoever for the person you granted it to (#667). The post stayed
  missing from their browse list and `GET /posts/{id}` still refused it, because no
  read path had ever consulted the grants table — share was a button that stored a row.

  A live grant now opens the post on both read paths at once: the link you send opens
  for them, and `GET /posts` hands the post back when asked for the tier it sits in.
  Grants are purely additive, per ADR 0010 Layer 6 — a share can only ever open a post
  that was closed, never close one that was open, and nothing you can see today becomes
  invisible. `expires_at` works as advertised, so a time-boxed share stops granting the
  moment it lapses without anyone having to revoke it. Grants to a `user` principal are
  what read paths honour; `role` and `team` principals can still be recorded but do not
  grant yet, exactly as for collections.

  One thing a share still does not do, unchanged by this and tracked separately: it
  does not put the post in the recipient's default browse grid (that feed shows the
  walled-garden `org-only` tier and only that, whatever you have been granted).
  Searching for it *does* now work — #873, later in this same release, made the search
  rule the browse rule. No-spec-impact.

- **Admin pages no longer accuse administrators of lacking permission.** Opening an
  `/admin` page directly — a bookmark, a pasted link, a reload — showed a red *"You
  don't have permission to view this page."* panel for a moment before the page
  appeared (#871). Nothing was actually denied and no action ever failed; the console
  simply asked the server *who are you* and *what may you do* as two separate
  questions, decided what to show the instant the first answer arrived, and had to
  correct itself when the second one landed. On a slower connection the wrong answer
  was on screen long enough to read, and long enough to file a bug about.

  Your permissions now arrive with your session, in the same response, so there is no
  moment at which the console knows who you are but not what you may do. The dedicated
  `GET /auth/me/capabilities` endpoint is unchanged and still published — this changes
  when the console learns the answer, not what the API offers. Wire change:
  `CurrentUser` (returned by `/auth/me`, `/auth/login` and `/auth/register`) gains a
  `capabilities` array of your global capability codes. No-spec-impact.

- **Your default browse view is now the view you get.** Account → Preferences has
  offered a home tab, a browse layout and a browse sort since 1.17.G. All three saved,
  survived a restart, and were read by nothing: browse hydrated purely from that
  browser's `localStorage`, so the setting changed what the preferences page said and
  nothing else (#706).

  They now seed the browse view — and only seed it. The rule is *explicit local choice
  beats the account preference beats the built-in default*: a device that has never had
  its view changed by hand opens in your account's layout, tab and order, while a device
  where you picked masonry stays on masonry through reloads even if the account says
  grid. Changing the view while browsing is still a local act and never rewrites the
  account setting; that stays a deliberate visit to the preferences page. Signing in
  applies the settings immediately, without a reload. `feed` — the single-column layout
  phones already default to — is selectable as an account default for the first time.
  No-spec-impact.

- **Preferences no longer offer views the server cannot produce.** The home-tab picker
  listed **Trending** and **For you**; the sort picker listed **Popular** and
  **Trending**. None of the four existed anywhere behind the API — `GET /posts` accepts
  only `latest` and `following` and takes no ranking parameter at all — so choosing one
  saved a durable preference, flashed "saved", and left you on the plain latest feed
  under a label that promised otherwise (#736). All four are gone; the remaining options
  are exactly what the app can serve. Ranking is a feature that needs a model, not a
  label, and a guessed one is worse than none.

  An account that already stored one of the removed values is not an error. The stored
  string now reads back as "use default", on both the preferences page and the browse
  view, and saving anything clears it for good.

- **Your theme follows your account, not just your browser.** The light / dark / system
  choice was written to a cookie and read back from it, so it stopped at whichever
  browser made it — signing in somewhere new put you back on the default, and the
  per-user `theme` the API had been returning all along was read by nothing (#677). The
  choice is now saved to your account and adopted by any device that has not been set
  by hand, with the same precedence as the browse settings: a browser where you have
  explicitly picked a theme keeps it.

  The cookie remains what actually paints the page, so there is no flash of the wrong
  theme: when a new device adopts your account's theme it writes the cookie too, and
  every load after the first resolves before the first paint. "System" is now stored as
  its own value rather than as the absence of one, so an explicit *follow my OS* travels
  between devices instead of being mistaken for never having chosen (migration `00033`).
  No-spec-impact.

## [v0.8.0] — 2026-08-03 — Operator configuration: field vocabularies, tree editor, site text, and email templates

### Security

- **Putting someone's work in a post or collection no longer makes it more visible.**
  A post carried a complete asset record for every member, gated by nothing. A
  collection carried the same fields flat. So attaching a **restricted** asset to a
  **public** post published its title, its description, its file extension, its exact
  byte size, its free-form metadata blob — EXIF, including GPS coordinates — and its
  thumbhash, which is a blurred rendering of the actual picture, to anyone who opened
  the post. Anonymous visitors included, for whom that asset does not exist at all
  anywhere else on the site (#883).

  A member you may not see is now a **placeholder**: a lock, the word *Restricted*, and
  the owner's display name. Nothing else. Not the title — that is the whole point, and
  it is why the fields are **absent** from the response rather than blanked, so there is
  no empty-versus-withheld difference to read anything off. The permitted key set is a
  closed list — the membership row's own columns, a `restricted` flag, and
  `owner_display_name` — checked by a test that asserts the payload is a **subset** of
  it. No column of the asset record can cross that boundary, including one added next
  year; denylisting the fields we know about today is exactly how the SSO `config` blob
  leaked credentials, two entries down this list.

  The placeholder is **visible, not hidden**. Dropping the member from the list would
  have been the smaller change and it is the wrong one: it conceals that a restriction
  exists, and there would be nothing to attach "request access" to (#881, next).

  Who sees a member is now decided in one place for the three surfaces that expose one —
  post contents, collection contents, and IIIF collection manifests — and it is the
  conjunction of the two rules an asset already lives under: could you have opened that
  asset on its own, **and** are you entitled to its content tier (ADR 0064). The IIIF
  manifest was leaking a restricted member's title as its label to every signed-in
  caller; the check there had only ever run for anonymous ones. It now omits such
  members rather than showing a placeholder, because a IIIF collection's entries are
  links that viewers follow and a placeholder would be a broken one.

  **Search counted them too, which is worse than showing them.** A post's search
  document absorbed the text of every member, so a public post containing a restricted
  asset was returned for a phrase that appeared only in that asset's title. Nothing in
  the response named the asset — the *result count* was the tell, and a stranger could
  walk a title token by token off it without ever being shown a field. Post documents
  now include only members that everyone can see, and — separately, because a filter is
  only worth as much as its refresh — a post's document is now rebuilt when a member
  asset is restricted, unpublished or renamed, which nothing did before. Renaming an
  asset used to leave every post containing it matching the old name indefinitely.

  Wire-format change: `PostMember.asset` is absent on a restricted member and
  `PostMember.restricted` is new; `CollectionResource`'s asset-derived fields moved out
  of `required` for the same reason and gained the same flag. On a member you *can* see,
  every one of those fields is sent exactly as before.

- **postcss bumped past a path-traversal advisory.** The web build's copy of `postcss`
  was 8.5.17, which auto-loads source maps in a way that can be pointed at files outside
  the project. Build-time only — nothing shipped to a browser was affected, and an
  instance running artist-alley was never exposed — but it is a dependency of the tool
  that produces what does ship. Now 8.5.25 (#848). No-spec-impact.

- **SSO provider credentials are no longer readable.** `GET /admin/system/auth`
  returned each provider's whole configuration blob verbatim — which is where the
  OAuth client secret, the LDAP bind password and the SAML service-provider private
  key live. Setting one of those needs `system.auth.write`; reading the endpoint only
  needs `system.config.read`, so the narrower write capability was protecting nothing.
  The three secrets are now write-only: the response carries `client_secret_set`,
  `bind_password_set` and `sp_private_key_set` booleans and never the values, to any
  capability, `system.admin` included. Everything that is not a credential still comes
  back in full — including the OAuth client ID, the LDAP bind DN and the IdP
  certificate, which look like secrets and are not (#718).

  The blob was free-form, and that is the part that got fixed rather than patched.
  Denylisting the field names we know today fails open on the first one somebody adds,
  so `config` is now a closed, typed, per-kind schema — OAuth, LDAP and SAML fields
  named after their own protocol vocabulary, with no free-form remainder in which a
  credential could hide. The AI provider record's `config` had the identical shape and
  is closed the same way; #711 had made its `api_key` write-only but left the map
  beside it returned in full.

  Two knock-on changes come with it. Saving the auth settings now merges each
  provider's stored secrets in from the database when the request omits them (or sends
  them empty), because the admin page can no longer echo back what it never received —
  without the merge, the first display-name edit would have wiped every stored
  credential. And a failure to read the current settings during that save now aborts
  the write instead of being tolerated, since it is the merge input rather than only an
  audit record. The auth page grew the per-provider fields it never had; a configured
  secret shows as "configured" and stays untouched unless you type a new one.
  No-spec-impact.

### Added

- **Metadata field administration is now a grantable capability.** The field-management
  surface (Admin → Content → Fields) has always accepted either `fields.admin` or
  `system.admin`, but `fields.admin` was never a real row in the capabilities table, so
  the grant tables' foreign key rejected any attempt to hand it out — in practice field
  admin was superuser-only, with no way to delegate it. `fields.admin` now exists and is
  granted to the built-in Admin role, so an operator can give a non-superuser role the
  ability to create, edit, and delete field definitions without also handing over the
  whole install (#804). No-spec-impact.

- **The emails this instance sends can now be rewritten without forking.** Every
  transactional email — the verify-your-address message, the "send a test email"
  check, the saved-search and activity digests, the catch-all notification — rendered
  from a template compiled into the binary, so an operator who wanted to change a
  subject line or reword a body had no way to. Admin → Content → **Email templates**
  now lists each email with what it ships as, an editor for the subject, the plain-text
  body and the HTML body, the exact set of fields that email makes available, a live
  preview rendered against sample values in a sandboxed frame, and Revert. Your version
  replaces the shipped one on the next send — on this instance and on a second instance
  sharing the database, no restart (#795, ADR 0081 §2).

  A template may only reference the fields listed for its email — a small, typed
  view-model of strings, numbers and the odd list of rows, assembled per event. That
  list is the safety boundary: a template that names a field the email does not carry is
  refused **when you save it**, with the field named, rather than quietly rendering to
  nothing when the mail goes out. And if an override ever does fail at send time, the
  shipped template renders in its place so the mail still goes. This closes the
  operator-overrides epic (#519); locale-specific templates (#289) and email branding
  remain future work.

- **Any wording the interface ships with can now be changed without forking.** Every
  visible string came from a catalogue compiled into the build, so an operator who
  wanted "Collections" to read "Libraries" — or who simply wanted to fix confusing
  wording — had to fork the project and rebuild it. Admin → Content → **Site text**
  lists all ~2,150 strings beside what they ship as, with search, a "changed only"
  filter, and a Revert on anything you have touched. Changes take effect on the next
  page load, including for signed-out visitors, and including on a second instance
  sharing the database — no restart anywhere (#794, ADR 0081 §1).

  An override is stored per string *and per language*, so changing an English label
  cannot silently un-translate a Spanish one. An English change does still back a
  locale that has no translation for that string, because that locale was already
  rendering the English text. Overrides are plain text — they are never rendered as
  HTML, which stays exclusive to rich-text fields (ADR 0085).

  A change naming a string that does not exist is **refused, not quietly stored**: the
  save fails and names the key it could not find. That is the one behaviour this
  feature could not ship without — an override that appears to save and then does
  nothing is worse than no override at all, and is exactly the failure #774 fixed for
  the strings themselves. The server enforces it against a copy of the shipped
  catalogue embedded in the binary, so it holds for anything calling the API directly,
  not just the admin page. Per-group wording and federation of any of this are
  deliberately out (ADR 0081 §1): site text is how *this* install speaks.
  No-spec-impact.

- **A rich-text field renders as formatted text.** `rich_text` is the one field type
  whose entire purpose is formatting, and it was the one type that lost it: a value of
  `<p>Cleared for <strong>internal</strong> use.</p>` appeared on the post metadata panel
  as exactly those characters. Paragraphs, bold, italics, bulleted and numbered lists,
  block quotes, sub-headings and links now render as what they are (#816). The input is
  still the plain textarea it was — no editor toolbar in this change.

  What made this worth doing carefully rather than quickly is that it is the first
  surface in the app that renders stored markup instead of stored text, which is the
  place cross-site scripting lives. The markup is reduced to a fixed allowed set —
  `p br strong em ul ol li blockquote h3 h4` and links — by one policy on the server,
  applied both when a value is saved and again when it is served. Everything outside
  that set is removed rather than shown as escaped source. Links are limited to
  `http`, `https` and `mailto`, so a `javascript:` link is dropped, and every surviving
  link gets `rel="noopener noreferrer"` whether or not its author wrote one.

  Sanitising on the way out as well as on the way in is the deliberate part: not every
  writer is the API. Seeded datasets, imports, anything edited straight against the
  database and — later — a value arriving from a federated peer all reach the same
  column, and none of them pass the endpoint's checks. Doing it on read means a stored
  value is never trusted, which also means existing values need no migration. Other
  field types are untouched: `text` and `longtext` still render literally, tags and all.
  No-spec-impact.

- **A hierarchical field's nested terms are editable now.** `country` ships with 24
  nations under 5 continents, and until now the admin fields screen would show you every
  one of them and let you change none. You could see `gb / United Kingdom` sitting under
  `europe` and there was no way to rename it, retire it, move it, or put a term next to
  it — the controls were wired to the top of the list, so a continent was editable and a
  country was decoration. Every control now reaches a term at any depth: rename, the
  deprecate / archive lifecycle with its "use instead" successor, add a term under
  another, add one beside it, and reorder within a branch (#779, #825).

  Moving a term to a different branch is a **Move** button and a list of destinations,
  not a drag — a drag between nested lists is unusable with a thumb, and this has to work
  on a phone. The list leaves out the term you are moving and everything under it, so the
  one move that would corrupt the vocabulary (dropping a branch inside itself) is never
  offered rather than refused after the fact.

  Nothing you have already catalogued moves with it. An asset stores the term, not its
  position, so renaming Europe or moving the United Kingdom under a different continent
  rewrites zero asset records and every one of them keeps resolving — the new position
  simply shows up the next time the asset is read. New terms are typed as names, not
  codes: type "New Zealand", see the `new-zealand` it will be stored as before you commit
  it, and get told immediately if that term already exists somewhere else in the tree.
  Flat option lists (`select`, `multi_select`) are unchanged.

  Two saves at once are still caught the way they were: the editor sends the timestamp it
  loaded, and a field somebody else changed in the meantime — including a field that grew
  a term because someone typed a new keyword during an upload — comes back as a visible
  conflict with the choice to reload theirs or overwrite with yours. No-spec-impact.

- **You can now type a keyword in.** The previous entry taught the server to accept a
  keyword it had never seen; nothing in the interface could send it one. Upload's metadata
  panel drew every multi-pick field as a list of tick boxes over the terms that already
  existed, and a collection's fields used a plain multi-select list — both of which can
  only ever re-send a term the field already had. So the one field designed to grow could
  only be grown from the admin screen, one term at a time, which is the workflow that made
  the feature necessary in the first place.

  Both places now use a search box with chips. Type, and it narrows the terms on offer as
  you go, matching on the display name or the stored form and ignoring case and stray
  spaces — the same rule the server applies when it saves, so what the box offers is what
  actually gets stored. Typing `LANDSCAPE` where `landscape` exists offers you the keyword
  you already have. On an open field, a term that genuinely matches nothing gets an
  explicit **Create "…"** row showing the stored form it will take. Nothing is ever created
  because you pressed Enter near it — a vocabulary that grows by accident is how a
  catalogue ends up with *sunset*, *Sunset* and *sunsets* meaning one thing. Closed fields
  never offer to create anything, and say plainly when a term is retired rather than
  showing an empty list. The box is keyboard-driven (arrows, Enter, Escape, Backspace to
  drop the last chip) and sized for a thumb.

  A field is marked as an open vocabulary from **Fields & metadata → edit**, where
  multi-pick fields grew a *Let values add new terms* toggle. It appears on multi-pick
  fields only, because that is the only type the server honours it on.

  On a post, multi-pick values now read as separate chips instead of one comma-joined
  line — a set displayed as a sentence is a set in which no individual term is findable
  (#831).

- **Keywords can grow — by typing one, and from the files themselves.** `Keywords` shipped
  with a fixed list of 17 terms and no way to add an 18th except the admin options editor,
  one at a time. That is a workable rule for a field like `Country`, and the wrong one for
  the field whose whole job is to describe what is actually in your catalogue. A field can
  now be marked as an **open vocabulary**: a keyword it has never seen is *added* rather
  than refused, keeping the words you typed as its display name. `Keywords` is the first
  field set that way.

  Matching happens before adding, on the term's display name as well as its stored form,
  ignoring case and stray spaces — so `Character`, `character` and ` character ` are one
  keyword, not three, and typing the display name of a keyword you already have picks it
  rather than duplicating it. Every other field stays exactly as strict as it was: a term
  a closed vocabulary does not offer is still refused. Retired terms stay retired on open
  fields too — choosing a deprecated keyword afresh is still refused, and a keyword whose
  name collides with an archived one is refused rather than quietly resurrected or turned
  into a near-duplicate.

  Files can now fill the field in as well. Photographs, exports and just about every
  cataloguing tool write keywords into the picture itself (the IPTC 2:25 tag), and until
  now nothing read them — the extraction pipeline had no way to write a multi-value field
  at all, so `Keywords` was left deliberately unwired. It is wired now: uploading a picture
  that carries keywords matches each one against the field, adds the ones that are new, and
  records the change in the asset's history with `iptc` as the source, so you can see what
  came from the file and what somebody typed. Re-running extraction over the same file
  changes nothing (#830, #789).

- **The hover slideshow now fills the tile instead of floating between black bars.** In the
  grid, a video's cover picture fills its tile — but starting the hover slideshow used to swap
  in a letterboxed strip over a near-black backdrop, so more than half the tile went dark the
  moment you looked closely. The slideshow now fills the tile exactly the way the cover does,
  showing the same central region. (Investigating this also cleared the cover image itself of
  suspicion — it was never the problem.) And for anyone who prefers reduced motion, hovering
  now simply keeps the cover picture instead of animating (#834, #837).

- **Animated GIFs now play their hover slideshow, and their thumbnail is a frame worth
  looking at.** A GIF was treated as a still picture: the system decoded the first frame,
  made a thumbnail out of it, and stopped. For a screen recording the first frame is
  usually the empty window before anything happens, so a library of animated GIFs showed a
  library of blank rectangles — and hovering one did nothing, because the little slideshow
  of frames that videos and 3D models get was never generated. Animated GIFs now get both:
  a thumbnail chosen the same careful way a video's is (a representative frame from a
  tenth of the way in, skipped forward if that one is nearly black) and the full hover
  slideshow. Still GIFs are unaffected and cost nothing extra — the system checks whether
  the file actually moves before doing any of the expensive work (#832).

- **Hover slideshows are twice as sharp.** Hovering a video or 3D model plays a little
  slideshow of frames; those frames were generated at half the resolution of the still
  image they replace, so the moment you hovered, the picture went soft. The frames are
  now half again larger, which removes most of the visible blur, while the number of
  frames — the smoothness — is unchanged. The extra storage this costs came in under
  what was budgeted when the trade was approved (#811).

- **A freshly uploaded video shows its picture in seconds, not after the whole transcode.**
  Making a video ready to stream is the most expensive work in the system, and until now a
  video card showed nothing at all — not even the blurred placeholder — until every bit of
  that work finished. Grabbing one good frame takes under two seconds, so that now happens
  first, on its own fast track, and the heavy streaming work follows behind at a lower
  priority. Uploading a batch of videos shows pictures appearing within moments while the
  transcodes queue up (#818).

  **Film covers stop being black frames.** The cover image used to be whatever was exactly
  one second into the video — for anything that opens with a fade from black, that is a black
  frame, and several films' cards were literally solid black. The cover is now chosen by
  scanning for a representative frame and checking it is actually bright enough to see,
  looking deeper into the video if the opening is dark (#810).

  **Restoring a database backup no longer blanks every card.** Databases and stored files are
  backed up on different schedules, and restoring an older database left the system holding
  thousands of finished renders it had no record of — so it announced no previews, showed
  placeholders everywhere, and every background job reported success. It now notices renders
  it already has and records them, repairing itself on the next pass instead of staying
  broken silently (#827).

- **The information written inside a photo now actually gets read — all of it, not just the
  first kind found.** A photo usually carries several layers of embedded information: what the
  camera recorded (exposure, capture time), what an editor or newsroom added (credit, country),
  and what the rights holder attached (a copyright statement). The system only ever read the
  first layer it recognised, which in practice meant the camera's — the other two were parsed
  by code that no upload could ever reach. Now every layer is read, and each is kept separate
  and labelled with where it came from (#800).

  Four of the built-in fields fill themselves in from those layers on upload: capture date,
  credit, copyright, and country. Two rules keep this trustworthy. **A value a person chose is
  never overwritten** — automatic extraction only fills fields that are empty. And **a country
  name found inside a photo is stored as its standard two-letter code** by matching it against
  the field's vocabulary; a name that matches nothing is reported as unresolved rather than
  guessed at or stored raw (#799, #813).

  Under the hood, a leftover column that once described this wiring but was never read is gone,
  so there is now exactly one place that says where a field's automatic values come from (#813).

- **The built-in Country and Keywords fields now come with a starting vocabulary.** Both
  shipped with an empty list of choices, which for a pick-list is the same as not working:
  Country was a hierarchy with nothing in it, and Keywords had nothing to pick. Country now
  offers a two-level starting set — continent, then country — and Keywords a short list of
  general-purpose terms. Both are explicitly starting points for an operator to extend or
  prune, not an authority (#820).

  Country entries are stored under their standard two-letter ISO country codes rather than
  invented names, so a value recorded on one site means the same thing on any other site it
  travels to.

  Along the way, two related gaps were fixed: loading sample content could not attach values
  to any of the built-in fields (only to the fields the sample library itself defines), and
  the field admin could not display a hierarchical field's choices at all — it showed "a tree
  field has no option list" even when one existed. Editing nested entries is still to come
  (#825), and the system does not yet reject a value that isn't in a field's vocabulary
  (#824).

- **A field can now be a hierarchy, and the sample library exercises every kind of field
  there is.** Fields come in eleven kinds — plain text, long text, formatted text, numbers,
  yes/no, dates, timestamps, pick-one and pick-many lists, a hierarchical category, and a
  pointer to another asset. Six of those had never carried a value in any sample library, and
  the hierarchical kind could not even be *described* in one: the file that defines the sample
  fields could only express a flat list of choices, so a category with sub-categories was
  unwritable. That is fixed, and the sample library now defines and fills all eleven (#808).

  The sample media was regenerated to match: every video trimmed to two minutes, keeping its
  format and audio and subtitle tracks intact, and a deliberately rotated photograph added so
  that orientation handling is exercised by something real rather than by a square test image
  no camera would produce (#805).

  **This immediately paid for itself.** Filling in kinds of field that had never held data
  exposed four faults that no test could have found, because the situations they occur in did
  not exist: a malformed pointer was being stored as an empty value rather than rejected;
  formatted text is displayed as raw markup; a date is shown a day early for anyone west of
  UTC; and a pointer shows an internal identifier instead of the thing it points at. The first
  is fixed, and the other three are now tracked.

- **Fields can fill themselves in.** A field can carry a default that is applied when an
  asset is uploaded, and a team can override that default for its own uploads — so a studio's
  work lands tagged as that studio's without anyone typing it. A default is either a fixed value
  or one of a short list the server works out for itself, like whoever is uploading or today's
  date. There is no scripting: a default is a value, not a formula (#793).

  It never overwrites anything. A value you typed stays, and so does one read out of the file
  itself — a default only fills a blank. A field offering a retired option cannot be given that
  option as its default.

- **Profiling can be turned on when something needs investigating.** Set `AA_PPROF_ADDR` and
  the server exposes Go's standard profiling endpoints on that address — off by default, on its
  own listener, published by no deployment file, and it warns loudly if pointed anywhere other
  than loopback. Profile data can contain anything the process is holding, so it is deliberately
  not something that is simply on (#781).

- **Controlled vocabularies are editable.** The options behind a `select` or
  `multi_select` field could only be set when the field was created — after that they were
  frozen, and `/admin/fields` had no edit surface at all. There is one now: add a term,
  relabel one, reorder them.

  **Terms are retired, not deleted.** Deleting an option that assets already reference would
  orphan those values, and the orphan shows up as a blank on an asset nobody touched — so the
  editor does not offer deletion. Instead an option can be marked *deprecated*, which stops it
  being offered for new values while everything already carrying it keeps resolving and
  displaying normally, and it can name a successor that the editor then suggests in its place.
  *Archived* is the harder retire, for terms that were mistakes rather than terms that were
  superseded (#737).

  Saving is now conflict-checked. Changing one term rewrites the whole vocabulary, so two
  admins editing different terms used to silently overwrite each other — the loser never found
  out. The save now carries the version it was based on and is rejected if the field moved
  underneath it, offering to reload or to overwrite deliberately. Fields whose options nobody
  has edited are left byte-for-byte as they were.

- **Operators can set their own instance logo.** The mark in the navbar and on the
  sign-in page is no longer fixed to the icon that ships. Upload a PNG, JPEG, GIF or
  WebP under `Admin → Themes & branding` and it applies everywhere immediately; the
  shipped default returns the moment you clear it, and is always one click away
  (#517).

  The last five logos are kept so a previous mark can be picked back up without
  re-uploading it — including when the original file is long gone, which is the case
  the list exists for. Re-selecting one moves it to the front rather than adding a
  duplicate, and a sixth upload drops the oldest. Every listed logo is retained in
  storage for exactly as long as it is listed, so an entry the picker offers is an
  entry that actually still works. If a logo's image data does go missing anyway —
  a database restored against a fresh bucket, say — the picker says so in words and
  refuses to apply it, rather than showing a broken thumbnail or swapping your
  working logo for one.

  Uploads are treated as hostile input, because a logo is an operator-supplied file
  rendered on every page: the file must decode as a real raster image, its type is
  taken from decoding it rather than from anything the browser claimed, and it is
  capped at 2 MB and 1024px per edge. **SVG is not accepted** — it is an executable
  document format, and accepting one safely needs a sanitiser that is its own piece
  of work rather than a detail of this change. Rasterize vector art before uploading
  it. No-spec-impact.

### Changed

- **Internal release numbers no longer show up in the interface.** "Coming soon" spots
  across the admin console and the account area — the disabled tiles for features not yet
  built, several federation notices, the asset-type and AI intros, the placeholder shown
  in the asset viewer for file types without a dedicated viewer, and the pending
  whiteboard tools — used to carry the internal roadmap identifier for when the feature
  was scheduled (for example "Phase 1.22.C", "1.18.B-12", "C-1.14b"). Those meant nothing
  to an operator or a user and are gone; a not-yet-built area now simply reads as coming
  in a future release. The internal codenames also disappear from the federation copy
  (an "aa:Share" grant is now just a "share" grant). Dev-facing source comments are
  unchanged (#801). No-spec-impact.

- **The browse feed's "Team" and "Trending" buttons are gone; the filter is now
  Latest / Following.** Both removed buttons were decoration: neither value was ever
  in the server's `feed` enum, so clicking them sent a query param the API ignored and
  handed back the plain latest feed — the same posts, under a label that promised
  something else. Nothing is lost because nothing was there. Neither returns by
  re-adding a button: `trending` needs a ranking model (recency against engagement,
  and how fast it decays) decided first, and `team` returns with the teams browse
  surface (#684), where the team-scoped query gets designed once. A browser that
  remembers "Team" or "Trending" from before opens on Latest (#691).

### Fixed

- **Re-seeding an instance no longer leaves it serving stale data until a restart.**
  `aa seed --reset` wipes and rebuilds the content in the database, but a running app
  keeps its in-memory caches — so after a reset it went on answering from the pre-reset
  copy (old titles, deleted assets, missing new ones) until the process was restarted.
  The seeder is a separate process and cannot reach into the app's caches directly, so on
  `--reset` it now broadcasts a single flush over the same Postgres notification channel
  the caches already listen on, and every running instance drops all of its caches at
  once. The reseeded data shows up immediately, no restart or redeploy required. This is
  what lets the public demo reset cleanly (#845). No-spec-impact.

- **A reference field can no longer be pointed at an asset that does not exist.** A
  `reference` value is a bare asset id, and the write path accepted any id at all — so
  saving one that named nothing (a typo, a since-deleted asset, an id from another
  instance) returned success and stored a link to nowhere. The write is now refused with
  422 `dangling_reference` unless the target resolves. This is a WRITE gate only, and
  deliberately so: a reference that was valid when you saved it and whose target is
  deleted *later* still reads fine, degrading to the bare id with no fuss (the behaviour
  #839 chose) — deleting an asset does not retroactively break every record that pointed
  at it. Both the asset and the collection write paths enforce it identically (#842).

- **Collection metadata now shows labels and titles, not raw slugs and ids.** A pick-list
  value on a collection rendered its stored slug (`in_review`) instead of its label ("In
  Review"), and a reference value rendered a 36-character id instead of the linked
  asset's title — while the same field on an *asset* rendered both correctly. The
  collection read path simply never resolved them: the asset query joined the field
  definition and the referenced asset, the collection query did not. It does now, through
  the same query shape and the same shared formatter, so the two subject kinds render
  identically. A reference whose target has been deleted degrades to the bare id with no
  disclosure, exactly as on the asset side (#840).

- **Field-value history rows no longer outlive the asset or field they describe.**
  `asset_field_value_history` had no foreign keys, so a deleted asset or field left its
  history behind as orphan rows pointing at ids that no longer existed — and those rows
  survived `aa seed --reset` too, since a cascade cannot reach a table nothing references.
  It now carries the same two `ON DELETE CASCADE` foreign keys its collection counterpart
  always had, on `asset_id` and `field_id`; deleting either takes the history with it. The
  migration deletes any pre-existing orphans before adding the constraints, and the bespoke
  history sweep the reset used to run for this table is gone — the constraint does the job
  now (#821). No-spec-impact.

- **An upload no longer reports success when the server refused your metadata.** Per-file
  field values are written after the asset exists, and the upload modal sent those writes
  without ever looking at the answer. Every refusal — a term the field does not offer, a
  value in the wrong shape, a server error — was discarded, and the upload went on to
  report success while the value silently went nowhere. The comment excusing it said you
  could set the value from the asset page later; there is no asset edit page yet, so what
  was actually being dropped was dropped for good.

  Refusals are now read and shown on the file's row, one line per field, naming the field
  and saying what was wrong with it — and submitting stops so you can see them, rather
  than clearing the screen that was reporting the problem. Fix the value and submit again.
  Every field is still attempted, so one bad value shows you the rest of the problems
  instead of hiding them behind the first (#843).

- **A value that isn't one of a field's choices is now rejected instead of silently stored.**
  Writing to a pick-list, multi-pick, or hierarchical field with a term that isn't in the
  field's vocabulary used to succeed — the bogus value was stored, never resolved to a label,
  and displayed as a raw code forever. It now gets a clear rejection naming the field and the
  offending term. Retired terms follow the same rule the editor already uses: they can't be
  newly chosen, but a record that already holds one keeps it, and editing *other* parts of
  that record isn't blocked by it (#824).

- **Dates stop being a day early, and "derived from" names the actual asset.** A field that
  holds a calendar date (like a licence expiry) was being converted into the viewer's timezone,
  so anyone west of UTC saw the previous day. Calendar dates now display exactly as recorded,
  in the unambiguous `2026-10-22` form; fields that hold a real moment in time (like an ingest
  timestamp) still show in local time. And a field that points at another asset now shows that
  asset's title as a clickable link instead of an internal identifier — pointing at something
  that was later deleted degrades gracefully rather than breaking the panel (#815, #817).

- **Hovering a short clip no longer scrolls through blank frames.** The hover slideshow
  always stepped through a hundred frames, however many the clip actually had. A five-second
  video only has about twenty-five, so three quarters of the hover was empty black — and
  the shorter the clip, the more of it was nothing. The card now plays exactly the frames
  that exist. Nothing needs regenerating: the information was already stored alongside every
  slideshow, the card simply was not reading it (#835).

  Two related things fall out of the same change. The slideshow is no longer switched on by
  file extension — the card now asks the server whether this particular asset has one — so a
  video whose full processing has not finished yet stops requesting frames that are not there
  (previously a silent failed request), and any format that grows a slideshow later works
  with no further change. Animated GIFs are the first beneficiary (see #832 above).

- **A new installation starts with a small set of ready-made fields, and re-loading the
  sample library no longer deletes them.** Every installation has always been created with
  a handful of standard fields — title, description, credit, copyright, capture date,
  keywords, country, and the two that record an image's pixel dimensions. But loading the
  sample library wiped them and put its own fields in their place, so the arrangement an
  operator actually starts from was the one nobody ever ran. Loading sample content now
  adds to that set instead of replacing it (#812).

  **Six fields that were never meant to ship have been removed.** They came in with the
  original database snapshot — leftovers from testing, carrying a user reference no
  installation has — and appeared to every operator as *Text Field*, *Score*, *Due*,
  *Tags*, and two called *Fed Guard*. They are gone, along with any values recorded against
  them.

  A companion note for anyone tracking the details: the history of past edits to a field's
  value was never being cleared when its asset or field was removed, so it accumulated
  indefinitely. That is now cleaned up as well.

- **A seed run now says when it throws a field value away.** Loading a catalogue could
  discard a value in two different situations — the file named a field that does not
  exist, or it carried something that field's type cannot hold — and both were silent.
  The run reported success, the field was simply missing afterwards, and any check that
  counted rows agreed that everything was fine. A seeded instance could therefore be
  quietly missing data that nobody had any way to notice.

  Both cases now report themselves, and they report themselves *differently*, because
  the two need different fixes: one is a mismatch between the catalogue and the
  definitions, the other is a bad value. A run ends with a plain-language note saying
  how many values were dropped and which fields they belonged to. A single misconfigured
  field no longer floods the log either — a few examples are shown, while the count
  stays exact (#807).

  A date field also accepts an ordinary calendar date now, such as `2026-03-14`.
  Previously it required a full timestamp and would discard anything else — without
  saying so, which is precisely the trap above.

  While fixing this, one worse case turned up: a reference field given a malformed
  identifier was not being discarded at all. It was accepted, and stored a value that
  was empty in every respect — a field that reads as "deliberately set to nothing"
  rather than as absent, and the one shape a row count cannot detect. It is now refused
  like any other bad value.

- **A portrait phone photo would have tiled as a landscape one.** Two different parts of
  the system were writing the tile shape a browse page reserves for an image, and they
  disagreed for exactly one kind of file: a photo taken in portrait, which cameras store
  landscape with a tag telling the viewer to turn it. The preview pipeline recorded the
  shape you actually see; the metadata reader recorded the shape on disk, and it wrote
  last, so the wrong one won. Only the preview pipeline records it now (#765).

  Nothing in the catalogue was visibly wrong yet — the six affected rows were test
  images — so this is a trap removed before the first real phone photo hit it rather
  than a repair. The shape is now measured in one place, from the same image the
  thumbnails are built from, which is also the only place that can answer the question
  for the half of the catalogue with no source pixels at all: a 3D model, a font, an
  audio file and a document each produce exactly one picture on the way through, and its
  shape is what a tile reserves. No-spec-impact.

- **Portrait video previews were squashed into landscape.** The strip of thumbnails you
  scrub through was built at a fixed widescreen shape whatever the video actually was, so
  anything shot on a phone came out stretched. Thumbnails now keep the source's proportions —
  including the case that matters most in practice, a clip whose file says it is landscape and
  which plays portrait because of how the camera recorded it (#761).

  The same squash was happening on the browse grid, where hovering a video card scrubs through
  the same strip.

  **And in the viewer, the hover preview was not appearing at all** — it was being clipped to
  the height of the scrub bar itself, twelve pixels, ever since the scrubber gained zoom. It is
  a separate fix, and it is why the problem looked like it only affected the grid.

  Existing videos keep their old thumbnails until their previews are rebuilt.

- **Yes/no fields never worked either, and one of them failed louder than blank.** A field can
  be declared as a yes/no checkbox, and the parts of the system that write one disagreed about
  how. Setting one on an asset stored a number, while the panel that displays it looked for the
  words "true" and "false" — so it showed nothing. Setting one on a collection stored the words,
  in a different place from where an asset's went. And ticking the box in the upload window sent
  the words to a destination that only ever accepted the number, so that write was **rejected
  outright** rather than merely rendering blank — the one part of this that would have shown a
  user an error rather than an empty row.

  Ten places, and the two that were right were the ones nobody looked at. It survived for the
  same reason the hierarchy bug below did: no yes/no field has ever existed on a real instance,
  so the whole path had never once been run. A number, 1 or 0, everywhere now — which is what
  the design document said before any of the disagreeing code was written. Nothing to migrate:
  there was no stored value anywhere, in either form (#791).

  "Not set" and "no" stay different: an unset field shows nothing, a field set to no shows "No".

  The test that guards this used to check only *where* a value is stored. Two writers can agree
  on that and still disagree about what they put there, which is exactly what happened, so it
  now drives the writers with the same input and compares what each one actually produces. The
  list of tolerated exceptions is gone rather than emptied — an exception list is somewhere to
  put the next one.

  With this, all eleven field types agree across every writer and every screen, and the two that
  had never been exercised end-to-end now have a test that creates one, sets it, and reads it
  back out of the database.

- **Hierarchical fields never worked, and nothing had noticed.** A field can be declared as
  a hierarchy — country / region / city — and every part of the system disagreed about where
  such a value was stored. An asset put it in one place, a collection in another, and the panel
  that displays it read a third, so the value came back blank whichever way it had been entered.
  Two more places could not resolve a nested term at all, having only ever looked at the top
  level of the list.

  It survived because no hierarchy has ever held a value — a fresh install ships one, wired to
  read the country out of a photo's embedded metadata, and it had simply never fired. Settled
  now: a value is the single term it points at, and the path above it is worked out on the way
  out. Renaming a term, or moving one to a different parent, leaves every asset untouched (#778).

  There is a test that fails if any of the eight places disagrees again.

- **Five screens showed internal key names instead of English.** The AI configuration and
  AI usage pages under Admin displayed text like `admin.ai_inference.budget_hard_label` where
  their labels and help text should have been; the similar-assets panel, the collection field
  editor and the tag-source tooltips in the viewer had the same problem. The wording had been
  written all along — the screens were simply asking for it under the wrong name. Forty-six
  strings, now resolving (#774).

  Spanish and French translations moved with them, so nothing regressed to English.

  The reason this survived: the test meant to catch it only checked that text was *marked for
  translation*, never that the translation existed — so a screen asking for a name nothing
  answered to passed cleanly. It now checks that every requested name resolves, which is what
  turned up two of the five screens nobody had reported.

- **The server could be killed by its own memory limit while building previews.** Go decides
  when to collect garbage from how much memory is already in use, and it has no idea a container
  limit exists — so on a machine with plenty of RAM it would let the heap grow past the
  container's ceiling and be killed for it. Generating previews for large images is where that
  bit: resizing one holds a scratch buffer proportional to the image's dimensions, and several
  run at once. Symptom was the whole instance going unresponsive for a minute or two mid-render,
  health checks included, then recovering.

  The runtime is now told the container's own limit at startup, read from the container rather
  than written down somewhere that could drift out of step with it. Requests that previously took
  nearly five seconds during a heavy render now take a seventh of a second (#781).

  The default deployment also gains a memory limit, which it never had — only the CI
  configuration capped anything. That meant the failure was reproducible in CI and invisible to
  operators. Override with `AA_APP_MEM_LIMIT` if your host warrants something other than the 4 GB
  default.

- **Per-job-type concurrency limits were not actually limiting anything.** An operator can
  cap how many jobs of a given kind run at once — transcription at one, video and 3D previews
  at two — precisely because those are the jobs heavy enough to hurt a machine when several run
  together. The check and the counter were separated: a worker asked "is there room?", and the
  answer was only recorded once the job had been claimed. Every worker that asked during that
  window got the same stale answer and the same yes, so the real ceiling was however many
  workers happened to be polling — not the configured number. Observed in the wild: five
  concurrent 3D renders against a limit of two. The reservation is now taken at the moment the
  check passes, so a limit of two means two (#777).

  The old comment described this as a window that "can let one extra job through." A test that
  releases sixteen workers into it against a limit of two saw **all sixteen** admitted.

- **Vocabulary values showed their internal slug instead of their label.** A term is
  stored by slug so that renaming it is free and rewrites nothing on any asset — but only the
  editing screens ever turned that slug back into the label, because they happen to load the
  field definition to build their dropdown. Everywhere else, including the post and asset
  detail panels most people actually read, the raw slug came through. Labels now resolve on
  the server, so every surface gets them and none has to know a controlled vocabulary is
  involved. A term with no label of its own still shows its slug, unchanged (#775).

  This also completes the deprecation marking added above: a retired term now reads as
  deprecated on the detail panel, not only inside the picker.

- **Long metadata values were unreadable on a phone.** The detail panel's two-column
  layout gave the value column roughly two characters at 390px, so `N/A` broke across lines.
  It stacks below the small breakpoint now (#775).

- **Every dropdown in the collection field editor was empty.** The vocabularies were
  stored correctly and the editor could not read them: it expected each option to be an object
  and they are stored as plain strings, so it rendered one blank row per term. The upload
  form, which read the same data the other way, worked — which is why this survived. Both
  now accept either form (#737).

- **The admin tables were unusable on a phone.** At 390px the fields table was wider than
  the screen with nothing to scroll it, so the overflow was not merely off-screen — it was
  unreachable. The Save button sat at a negative x-coordinate with the document reporting no
  horizontal overflow at all. The tables scroll now (#737).

- **Masonry browse laid every tile out as a square.** The layout is supposed to respect each
  asset's real proportions, and it could not: nothing in the system had ever recorded a
  source's pixel dimensions. The `pixel_width` / `pixel_height` field definitions existed and
  every value was null across the whole catalogue, so the estimator fell through to 1:1 for
  every tile and masonry rendered as a plain grid. The preview pipeline now records the source
  dimensions as it decodes — it is the only producer that runs for *every* asset type, so this
  also covers the tiles EXIF could never describe: audio waveforms, video posters, SVG and 3D
  turntables, which are precisely the ones that looked worst squared off (#757).

  The same missing data was breaking IIIF for the entire catalogue. `info.json` is built from
  the asset's dimensions, and a 0×0 asset is rejected as unsupported — so every IIIF request
  had been 404-ing. It serves now. The UI test covering that endpoint had been asserting a
  JSON content type and passing *because* of the 404, which is a test defending the bug rather
  than the behaviour; it now asserts the IIIF media type.

- **nginx kept routing to a dead container address after any recreate.** Hostnames inside an
  `upstream` block are resolved once, when the config loads, and a `resolver` directive does
  not change that — the `resolve` parameter that would is NGINX Plus only. So when the app
  container came back with a different IP, nginx went on proxying to the old one. On the
  two-instance dev stack this surfaced as the worst possible symptom: traffic for one site
  silently served from the other, which reads as data corruption rather than a routing fault.
  The `upstream` block is gone; `proxy_pass` now goes through a variable, which defers
  resolution to request time (#756).

  Removing the block also removes connection reuse to the app — `keepalive` cannot be
  configured without it. That cost is recorded in the config beside the change rather than
  left to be rediscovered.

- **3D thumbnails rendered untextured while the viewer showed the materials.** ADR 0069
  chose headless three.js precisely so the browse-grid thumbnail and the interactive
  viewer would match, and both files said they shared code. They did not: the headless
  page reimplemented the viewer's loader, and the copy had no `MTLLoader` and no
  material-upgrade pass. So every OBJ rendered as untextured white (its `.mtl` was
  never fetched — OBJ references materials by name and the loader does not follow it),
  and every `KHR_materials_unlit` glTF rendered as a flat, unshaded silhouette, because
  the `MeshBasicMaterial` that extension produces ignores the entire lighting rig. The
  thumbnail also captured its frames before the textures had finished decoding, which
  would have kept OBJ untextured even once the `.mtl` loaded.
  The load path — loader choice, `.mtl` resolution, material normalisation — is now one
  module both surfaces import, so the drift cannot recur, and the release-image smoke
  test asserts materials came out normalised and the textured fixtures came out
  textured. Previously it only asserted the poster had non-transparent pixels, which is
  why an untextured catalogue shipped green twice: flat white geometry passes that
  check. Existing thumbnails are regenerated by re-running the preview job for an asset
  (#689). No-spec-impact.

  Correction: re-running the preview job did NOT regenerate anything until #760 landed,
  so this fix — and the two companion fixes below it — were invisible on every existing
  install. See "Recreating a preview now actually re-renders it".

- **Recreating a preview now actually re-renders it.** "Recreate previews" enqueued a
  job, returned a job id, and changed nothing. Every preview worker skips outputs that
  are already in storage — which is what makes an ordinary re-queue nearly free — and
  nothing could tell them not to. So the job ran, skipped everything, and completed
  successfully, and the thumbnail stayed exactly as it was. Three shipped renderer
  fixes (#689, #750, #753) therefore reached no existing asset: the only way to see
  them was to upload the file again under a different hash. The endpoint now forces a
  re-render by default, and `POST /assets/{id}/preview?force=false` selects the old
  gap-filling behaviour deliberately (#760).

  Nothing is deleted first. Each output is overwritten in place by an atomic write, so
  an interrupted rebuild leaves the previous — stale, but present — preview serving
  rather than an asset with no thumbnail at all.

  `aa rebuild-previews --ext glb,fbx,obj` re-renders a whole set, because the situation
  that produces this is "a renderer changed and every asset it ever touched is stale",
  which is not a click-once-per-asset problem. It reports how many of the assets it
  swept already had renders, i.e. how many of those jobs are replacing bytes rather
  than filling a hole. `scripts/preview-backfill.sql`, which could only enqueue
  raster jobs and whose own comment noted that a second run did nothing, is gone.

  Two smaller honesty fixes came with it. `storage_variants` gained `updated_at`: the
  table previously recorded only when a variant was FIRST written, so a successful
  re-render was invisible in the database and "have these bytes changed?" had no
  answer. And a completed job now logs what it wrote and what it skipped — `590 jobs,
  0 failures` used to look identical whether 590 renders happened or none did.
  No-spec-impact.

- **`aa seed` says when the previews it queues will do nothing.** `--reset` truncates
  the content tables but deliberately leaves the content-addressed variant store alone
  (the blobs are on the volume; a database truncate does not erase them, and pretending
  otherwise produced assets with no thumbnails at all). Re-seeding the same dataset
  therefore enqueues a preview job per asset whose output already exists, and every one
  of them skips. That was correct and silent — the seed reported "previews queued: 590"
  either way. It now reports how many of those will skip and points at
  `--force-previews`, which re-renders them (#760). No-spec-impact.

- **363 of 374 3D models were missing their textures, because "a GLB embeds everything"
  was written down as a fact and is not one.** A `.glb` is a binary wrapper around
  ordinary glTF JSON, so it can point at `Textures/foo.png` on disk exactly as a `.gltf`
  does — and almost all of ours do. Four separate places asserted the opposite: the Go
  companion resolver, the seeder that called it, and two dataset scripts, in one of which
  the claim had grown into a justification for how the library was assembled. Because the
  copier shared the belief, the texture folders were never even placed in the dataset, so
  fixing only the code would have found nothing and looked correct. The resolver now reads
  a GLB's JSON and asks, instead of assuming; the duplicated extension list in the seeder
  is gone, so there is one answer to "does this format have companions" rather than two
  that could drift apart. The test that should have caught this was checking an
  eleven-byte text file named `model.glb` — it passed because there was nothing to parse,
  and it kept the assumption alive; those same bytes are now a case that must fail.
  Existing thumbnails still need the preview job re-run to pick the textures up (#750).
  No-spec-impact.

- **The same was true of FBX: 105 of 109 named a texture and none of them found it.** FBX
  keeps its texture filenames inside binary node records, which nothing had ever read, so
  the comment saying FBX "embeds its resources" went unchallenged for the same reason the
  GLB one did. There is now a reader for it, and a matching one in the dataset copier so
  the texture folders actually get staged. Two subtler problems came out with it. Companion
  paths were being stored with the backslashes FBX writes, because the code normalised them
  with a call that does nothing on Linux — so `Textures\barrel.png` was one filename rather
  than a folder and a file, and nothing could match it. And the thumbnail renderer asks for
  textures by bare filename regardless of the folder they live in, so correcting the stored
  path was necessary but not sufficient; the renderer now resolves them explicitly. Proven
  by running the release-image smoke test with and without that step. As with the GLB fix,
  **existing thumbnails do not change until previews are re-rendered** — they are still the
  output of the renderer that shipped before July, which is tracked separately (#753).
  No-spec-impact.

- **Six seed images were showing a fraction of their own artwork.** The Kenney pack's
  Flash-exported sprite sheets carry a stale artboard — `viewBox="0 0 550 400"` on a
  drawing that actually spans 2248x1120 units — and the rasteriser honoured it, so the
  Platformer Pack Remastered background sheet shipped holding **8.8%** of its picture
  and the Physics Assets material sheets held 19.7%. They did not read as broken;
  they read as a legitimately cropped sheet, which is why two prior sweeps went past
  them. Every source that declares a canvas is now measured against what it actually
  draws, and reframed to its real extent when the two disagree. Sources whose canvas
  is correct — 800 of the pool's 806 — render byte-for-byte as before. `aa`'s pool
  builder grew `--rerender` so a rasteriser fix can reach a pool that already exists,
  instead of being skipped as "already on disk" (#685). No-spec-impact.

- **`detect_cropped_renders.mjs` is retired, not retuned.** It flagged 41% of a
  known-good pool, which trains everyone to ignore it — and that is how the above
  survived. Swept against ground truth over 9,504 combinations of its thresholds,
  alpha cutoff, agreeing-edge count and minimum pixel floor, **none** found all six
  genuinely lossy files and the best precision reached anywhere was 0.043. The signal
  is not there to be tuned: edge coverage measures a drawing's silhouette where it
  meets the frame, and that silhouette is identical whether the frame was right or
  cut. `seed/scripts/probe_render_loss.mjs` is the crop gate now — it compares the
  render against the source, covers all 1,031 pool sources rather than the 806 it used
  to, and no longer counts its own measurement boundary as lost artwork (#685).

## [v0.7.0] — 2026-07-28 — Browse correctness, visibility security, and a real seed catalogue

### Security

Four separate leaks, all found in one week and all the same underlying mistake: a
read path that wrote out the "who may see this" rule itself instead of asking the
one component that owns it. Each copy was correct when written, then the shared rule
moved and the copy didn't. None was caught by a test. They are grouped here because
the pattern matters more than any one of them (#665).

The last entry below is a different thing — a permission that was too broad rather
than a rule that drifted — but it is the same data class, so it belongs here.

- **Anyone signed in could read anyone else's private posts.** Adding
  `?visibility=private` to the post list returned other people's private posts —
  title and body — while opening the same post directly correctly refused. No special
  role was needed; an ordinary account was enough. The list and the single-post view
  now run the *same* rule, so they cannot disagree again, and a test enumerates the
  visibility tiers from the database itself so a tier added later is covered without
  anyone remembering (#660).

- **Any signed-in user could read other people's private collections.** The IIIF
  manifest route returned a collection's name, description and full member list with
  no permission check at all for signed-in callers (#661).

- **On a public install, adding a tag filter exposed unpublished work.** Browsing
  anonymously with `?tag=…` returned draft, archived and restricted assets that the
  unfiltered browse correctly hid, because the tag-filtered branch was a separate
  query that never got the visibility rule. Measured on the reference install: 34
  items including 17 drafts, versus 5 after the fix (#657).

- **Related-asset and IIIF manifest routes leaked unpublished assets anonymously**,
  including using a draft asset as a similarity anchor. Also fixed the same drift
  pointing the other way: genuinely public collections were returning "not found" to
  anonymous visitors (#661).

- **Session IP addresses are now a separately granted permission.** Viewing a user's
  sessions in the admin area required only the ordinary "read users" permission, yet
  it showed raw client IP addresses — personal data that the audit log had already
  been gating behind a dedicated permission. The two now match: the session list is
  still visible, the addresses need the additional grant, and a new decision record
  fixes the naming so the next surface carrying personal data doesn't invent a third
  standard (#573, ADR 0072).

### Operator-facing changes

- **The server image is half the size: 3.64 GB → 1.82 GB.** Blender is no longer
  packaged. It was 1.3 GB of the image — roughly a third — plus the ten X/GL
  libraries it loaded at startup, and since the three.js renderer landed (#498)
  nothing in the product invoked it: every 3D format in the reference catalogue
  (`glb`, `obj`, `fbx`, `gltf`) already rendered through the three.js worker, and
  a search of the whole catalogue for the Blender-only formats returned nothing.
  `stl`, `ply` and `dae` moved onto the worker with this change so they keep
  their thumbnails. Formats with no three.js loader (`.blend`, `.usd*`, `.abc`,
  `.x3d`) get no generated thumbnail for now — the file itself still uploads,
  downloads and serves normally — and regain one when the Blender converter
  ships as an optional plugin (#499). Nothing to do on upgrade; no configuration
  changed. (#500, ADR 0069 amended.)

  Two smaller consequences worth knowing: arm64 deployments are unaffected
  because they never had Blender in the first place (its tarball is x64-only) —
  they have had 3D previews since #498. And the `AA`-side escape hatch that
  forced the old renderer is gone; with one renderer there is nothing to switch
  to. Nobody had it set.

### User-facing changes

- **The seeded demo library now has eleven working studios instead of one.**
  site_a shipped 1,007 assets in which **Animation and Characters had none at
  all**, Marketing Art had 3 and Textures had 8, while Environment held **47.3%
  of everything**. Clicking a studio either showed an empty page or showed the
  whole dataset. It now holds **1,946** assets with every team between 116 and
  421, and Environment down to **21.6%** — a studio with a specialism rather
  than a studio plus ten placeholders (#572, closes #562).

  Two levers, because a floor alone would have left Environment at 37%. **55
  records were on the wrong team in the source data and said so in their own
  tags** — 34 minimap icons tagged `ui`, 18 tiling texture plates tagged
  `texture`, 3 voiceover clips tagged `voiceover` — and moving them to UI /
  Textures / Audio is a correctness fix that happens to cap the biggest team.
  The other 895 are new, drawn from the CC0 Kenney bundle the library already
  came from and of which only ~1.3% was in use. Nothing was deleted: posts,
  collections and sibling groups all reference those ids.

  The **floor is 60 per team, and it comes from the product** — `/search`
  returns 25 results a page and the browse rails render 24 tiles, so a team
  whose whole library fits in one response has nothing to scroll and nothing to
  narrow. It reads as a stub even when it is technically non-empty.

- **45 more video references, chosen to look like a game studio's.** Video
  coverage went from 47 clips to **92**. The additions are searched for
  deliberately — arcade cabinets, controllers and keyboards, neon and glitch
  plates, particles, smoke and sparks, pixel-art animation, esports floors —
  rather than generic stock, and each record records the search that found it.
  They land across Reference, VFX, UI, Marketing Art and Animation instead of
  piling into one bucket (#572).

- **Sponza renders instead of failing.** The canonical Khronos test scene was
  the one 3D asset in the instance stuck at `failed`, because its geometry
  buffer and 69 textures were never staged next to it — the copier attached a
  model's siblings only when it copied the model, so a model already present at
  the destination silently skipped its own companions. It now reaches `ready`
  with a turntable (#572, completes #486).

- **Assets with no preview picture now get a designed tile instead of a blank
  one.** Text and code files never get a rendered thumbnail, and a preview can
  also simply have failed — a 3D scene missing its geometry file, a photograph
  too large for the render cap. Both used to land on an anonymous grey landscape
  glyph that said "image missing" whether the asset was a CAD model, a README or
  a JPEG, which read as a broken tile rather than a deliberate one. The tile now
  states the two facts it actually has: the file's format, set as a wordmark, and
  its kind in plain language — `GLTF / 3D model`, `MD / Document`. Where the card
  does not already show the title next to the tile, the title is in it too, since
  a document is mostly its name. The tile composes itself to the space it has, so
  it holds up from a 60px masonry sliver to a full-width feed column. Rare on an
  image-heavy library — 3 of 1007 assets on the reference install — and much less
  rare on a document- or CAD-heavy one (#558).

### Accessibility

- **Form controls you could not see the edge of.** Inputs, selects and
  text areas drew their border in the same colour as a divider rule, which
  measured **1.38:1 in dark and 1.28:1 in light** against the surface behind it.
  On a divider that quietness is deliberate; on a control the border *is* the
  affordance — it is the only thing saying "you can type here" — and WCAG 2.2
  requires 3:1 for it (SC 1.4.11). 251 controls now use the strong border role,
  which was itself raised to clear the bar: measured on the rendered page,
  **1.38 → 3.98 (dark)** and **1.28 → 3.42 (light)**. Divider borders are
  unchanged; they carry no information and the low contrast there is intended.

- **Focus was easy to lose on those same controls.** 122 of them indicated
  focus by darkening that 1px border — a **1.95:1** change between the two
  states, and one that would have become invisible once the resting border was
  strengthened. They now draw the standard 2px focus ring, measured at
  **7.08:1 (dark)** and **3.39:1 (light)** against the page.

- **Secondary colour ramp fixed before anything used it.** Its white text
  measured 4.46:1 on the steel fill, under the 4.5:1 body-text floor. Now 4.85:1.
  No component is wired to this ramp yet, so nothing changes visually — the point
  is that the first one to reach for it does not ship a failure (#594).

### Fixes

- **The demo profile silently shipped 36 fewer assets than the studio profile it
  is a copy of.** `demo` and `dev` are aliases for `studio-a` and `studio-b`, but
  they were written before the dataset upgrade pass ran — so every upgrade since
  #604 landed on the studio profiles and missed its own aliases. A demo re-seed
  would have dropped all 36 added videos and nothing would have reported it. The
  aliases are re-copied after the upgrade, and a test asserts they match (#572).

- **A site could serve fewer posts than its dataset had.** `posts.json` was the
  one file the archive publisher never wrote — it was copied by hand — so site_a
  served 584 posts against a profile holding 859. The publisher now stages it
  (`--posts`), and warns when it is left stale (#572).

- **A missing source cache reported fully-staged assets as missing.** The
  internet-fetched cache is gitignored and usually absent on a machine that
  already has a populated site, so re-publishing reported 58 present-and-correct
  videos as MISSING and exited non-zero. Absence of a *source* is not absence of
  the *asset*; the check now confirms against the manifest's own byte count
  first (#572).

- **A few catalogue tiles showed a tiny graphic marooned in a big empty box.** The
  splat and line-pattern thumbnails rendered their artwork at about 1% of the tile,
  jammed into the top-left corner. Two separate things were wrong. The images the
  instance was serving had been rendered before the earlier canvas fix (#630) and were
  never re-loaded, so the catalogue was still handing out the old broken pictures. And
  the fix itself only went half way: it measures each drawing on a fixed-size search
  frame, so the safety margin it leaves is a fixed distance in the drawing's own
  coordinates — fine for a big drawing, a quarter of the picture for a small one. Small
  vectors came out filling half their frame. The renderer now measures a second time at
  the drawing's own scale, so the frame is tight whatever the size, and the 110 affected
  images were re-rendered and re-loaded. Images that were already correct are unchanged,
  including sprites whose source deliberately declares a padded canvas — those keep
  their padding. A new checker (`seed/scripts/detect_oversized_canvas.mjs`) measures how
  much of a frame the artwork actually fills; the existing one only sees artwork cut off
  at an edge and reads this failure as healthy (#672).

- **Resetting the demo left stale rows pointing at content that no longer existed.**
  `aa seed --reset` empties the content tables with `TRUNCATE ... CASCADE`, and CASCADE
  only follows foreign keys — so any table that names its target by a *kind + id* pair
  (which cannot have a foreign key) kept its rows while the things they referred to were
  deleted. Notifications about vanished posts, scheduled actions queued against deleted
  assets, workflow history for wiped assets, and featured placements for collections that
  were no longer there all survived every reset. Follower edges were worse: nothing linked
  them to the accounts they described, so each reset added a whole dataset's worth on top
  of the last (149 → 298 → 447 measured across three runs) while every earlier edge pointed
  at an account that no longer existed. A reset now finishes by deleting exactly the rows
  whose target is gone — rows that still point at something real, such as an action
  scheduled against the admin account, are left alone. Every such table in the database is
  now classified explicitly, with the storage pin table deliberately exempt, and a test
  fails if a new one is added without a decision (#569).

- **Search was broken for every signed-in user.** Every authenticated query returned
  an internal error and no results. A change months earlier had removed the "featured"
  flag from collections — featuring became a placement rather than a property — but the
  search query still asked for the old column, so the whole search failed rather than
  just the collection portion of it. Search works again; a test now pins the query
  against the real schema so a removed column cannot silently break it a second time
  (#650).

- **Masonry no longer reshuffles while you scroll.** Each time the feed loaded more
  results, the tiles you were already looking at jumped sideways into different columns.
  The layout balanced all columns by height across the entire list, so adding anything to
  the end genuinely changed where earlier items belonged. Tiles are now placed into
  columns as they arrive and stay put — loading more only ever grows one column downward.
  Measured: previously 30 of 36 visible tiles moved on each page load; now none do
  (#651).

- **Missing blur-up placeholders on posts.** Assets inside a post shipped without their
  tiny preview hash, so tiles popped in from blank instead of fading up from a blur, even
  though the data existed server-side (#648).

- **Masonry overlay controls no longer overflow thin tiles.** Giving each tile its true
  aspect ratio made audio waveforms genuinely thin — the narrowest measured 24px tall —
  while the selection checkbox and options menu need 44px each, so they spilled outside
  the tile they belonged to. Masonry tiles now have a floor tall enough to hold them,
  derived from the controls rather than picked; the overlay keeps only those two
  controls; and everything else about the asset moved into a tooltip that follows the
  cursor and sits outside the artwork it describes. The thinnest assets are slightly
  letterboxed as a result, which is the deliberate trade — a tile too small to click is
  worse than one slightly taller than its picture (#652).

- **Cards fetched images larger than the space they were drawn in.** The hint telling the
  browser how much room a picture would occupy advertised the largest size the install
  generates rather than the actual column width, so browsers downloaded oversized files —
  33% too large on a desktop wall, 113% on a phone. The hint now describes the real slot
  (#639).

- **A written post now returns the same shape a read does.** Creating or updating a post
  returned a response missing the preview availability, pixel dimensions and blur-up hash
  that every read path includes. Nothing visibly broke, because the app re-fetches after
  saving — but four such fields had quietly accumulated, and anything trusting the save
  response would have rendered a card with no picture and no placeholder. The same gap
  existed on two asset write paths and is fixed there too (#655).

- **Audio, 3D, video, fonts and ebooks had no blur-up placeholder at all.** The tiny
  preview hash was only ever computed when the uploaded file was itself an image, so
  every asset whose thumbnail is a *rendered* preview — an audio waveform, a 3D
  turntable, a video frame, a page render, a glyph specimen — had none, and its tile
  flashed blank before the picture arrived. Most visible on audio, which is both the
  largest group and the thinnest tile in masonry. Every preview format now computes the
  hash from the picture it just rendered, and a one-time sweep fills it in for assets
  already in the library — 618 of them on the reference install (#645).

- **The IIIF Image API returned 404 for every asset.** Both the image and
  `info.json` endpoints gated on `assets.has_image`, a column nothing in the
  codebase ever writes, so the condition was true for every asset and the whole
  Image API had been dead since it shipped — with no error, because "404" is also
  the correct answer for an asset that genuinely has no image. Image endpoints now
  serve real bytes, gated on whether a configured IIIF variant is actually stored.

  `info.json` had a **second, unrelated cause**, now also fixed: it reports an
  image's pixel dimensions, and nothing ever recorded them. The metadata
  extractor emits width and height, but no field definition existed to receive
  them, so the values were discarded and every `info.json` 404ed. The
  definitions are now seeded and wired to the extractor — on both fresh installs
  and existing ones. **The IIIF Image API is fully functional for the first time
  since it shipped** (#614, #618).

- **Widescreen art was square-cropped on cards.** Every card requested a
  single 320×320 centre-cropped thumbnail, because that was the only size
  guaranteed to exist. A 16:9 video or a wide illustration therefore displayed
  as a square — visibly disagreeing with its own hover preview, which used the
  true aspect ratio. Cards now pick an appropriately-sized image from the sizes
  this install actually generates, so wide art displays wide and large tiles
  stop showing upscaled thumbnails. The grid's contact-sheet view keeps its
  square crop, which is intentional (#502, #589).

- **Masonry now stacks tiles at their real proportions.** Previously every masonry
  tile was a fixed square, so a 16:9 video and a wide audio waveform were letterboxed
  into identical boxes and the view was indistinguishable from the grid. Tiles now
  follow each image's own aspect ratio — the space is reserved from recorded pixel
  dimensions before the image loads, so nothing jumps. The grid keeps its square
  contact-sheet tiles, which is intentional (#640).

- **Scroll position survives closing an asset or post.** Opening a post from deep in
  the feed and closing it returned you to the top, losing everything you had scrolled
  past. Position and loaded pages are now restored on the post, asset, collection and
  profile routes (#584).

- **The viewer's minimize button did nothing when the navbar was hidden.** Minimizing
  now brings the navbar back, with search usable, instead of leaving the viewer
  indistinguishable from its maximized state (#635).

- **Masonry view rendered as a single full-width column.** For five days the masonry
  layout showed one enormous tile per row instead of a multi-column wall — a CSS length
  property was given a percentage, which silently voided the whole declaration and fell
  back to "one column". Masonry now forms columns that track the tile-size control, on
  desktop and phone alike (#637).

- **Cropped artwork in the seeded catalogue.** Some vector-sourced thumbnails were
  missing chunks of their artwork — cut off mid-shape at the edges. The source files
  declare no canvas size, and the renderer was guessing one and clipping anything that
  fell outside it. 110 affected images were re-rendered; the renderer now measures each
  drawing's real extent first (#630).

- **Viewer gap when the navbar auto-hides.** Opening a post after scrolling far
  enough that the navbar had slid away left a navbar-sized gap above the viewer,
  with the feed's tiles bleeding through. The viewer's top edge was glued to a
  measured navbar height that never updated when the navbar hid (a transform,
  which resize observers can't see). It now tracks the navbar's actual state —
  expanding flush to the top of the screen when the navbar hides, and yielding
  the space again when it returns, with a matching animation (#628).

- **Regenerated previews never reached the browser.** Asset byte routes shipped
  `Cache-Control: immutable, max-age=31536000` with an ETag derived from the URL
  path — a validator that cannot change — and answered conditional requests with
  304 without ever consulting the stored bytes. Once a client had cached a
  variant, no sequence of requests could return updated content, so an operator
  who used "Recreate previews" after a renderer fix could never see the result.
  Validators are now derived from the stored bytes, and revalidation is permitted
  (#620).

- **EXIF metadata extraction processed zero assets.** The backfill selected on the
  same never-written `has_image` column, so a run would report success having
  enqueued nothing. It now selects on file format — the formats the metadata
  pipeline actually has an extractor for, EXIF plus camera raw. (Extracted values
  still need field definitions with an extraction source configured before they
  land anywhere; tracked in #618.) (#579)

- **Admin featured-content thumbnails.** The curation list at
  `/admin/content/featured` could not render a thumbnail for *any* subject:
  asset tiles were gated on the same never-written column (fixed with it), and
  collection tiles had no cover resolution at all — the public rail resolved
  covers since #559, but the admin list never received the same treatment.
  Operators now see real covers for both subject kinds, including team-tier
  covers the public rail rightly refuses to anonymous visitors (#619, #625).

- **AI asset hints never identified images.** The AI bridge derived its MIME hint
  from the same dead column, so it was never set. It now derives a real MIME from
  the file extension (`image/png` rather than the `image/*` wildcard it aspired
  to) (#579).

### Operator-facing changes

- **New capability `users.pii.read` — session IP addresses now need it.** The admin
  view of a user's sessions (`/admin/users/{ref}/sessions`, and the "Active sessions"
  panel on the user detail page) returned each session's raw client IP to anyone
  holding `users.read`, while the audit log has required a dedicated
  `system.audit.pii.read` for actor IPs since v0.5.0. Same data class, two different
  bars — so the looser one was raised rather than the stricter one lowered. `users.read`
  still lists the sessions, labels the devices, and revokes them; the address is
  additionally gated on `users.pii.read`, exactly as audit gates actor IPs. `system.admin`
  is unaffected (it satisfies every capability). **Operators who want an existing
  non-admin role to keep seeing session IPs must grant it the new capability** — the
  field is simply absent otherwise, never blank. Documented as a rule for every future
  IP-bearing surface in ADR 0072 (#573).

### API

- **Removed: `asset_has_image` from featured-item payloads.** It reported whether
  a raster thumbnail existed for a tile's cover asset, and it was **always
  false** — the underlying database column had no writer anywhere, in any
  install. Clients should use `preview_available`, which is computed from live
  variant existence and has been the trustworthy signal since it was added. No
  client behaviour changes, because nothing could have usefully depended on a
  field that was universally false. The column itself was dropped in the same
  change (#579).

- **`ladder_available` on asset payloads** — reports whether the *complete*
  configured preview ladder exists for an asset, so clients can build a responsive
  `srcset` instead of assuming a single thumbnail size. Computed against the
  operator's configured rungs rather than a hardcoded list, so an install that
  tunes its ladder is described accurately (#591).

- **`GET /previews`** — the rung keys and dimensions this install generates, so a
  client can build width descriptors without hardcoding defaults. Governed by
  public mode: anonymous on a public install, 401 on a private one (#591).

## [v0.6.0] — 2026-07-23 — Public read surface + demo hardening

### User-facing changes

- **Public user-profile pages.** Every user now has a profile page, reachable by
  username (`/users/by-username/{name}`) or stable ref, showing their display
  name, avatar, and the assets/posts/collections a viewer is allowed to see. It
  reuses the existing visibility rules — anonymous visitors see only public work
  (and only when public mode is on), and an owner can opt out of anonymous
  exposure. This also cleared the last of the dead author/similar-asset links
  (#478).

- **Shared view controls across every asset surface.** The browse view switcher
  (grid / masonry / thumbnail / list) and sort direction now appear on the
  profile and post-by-asset pages too, not just the main browse — one consistent
  control bar everywhere assets are shown (#511).

- **Faster 3D previews, and multi-file models fixed.** Open-format 3D previews
  (glTF/GLB, FBX, OBJ) now render through a headless three.js worker instead of
  Blender — much faster, and **arm64 deployments get 3D previews for the first
  time** (the Blender path was amd64-only). Multi-file glTF (a `.gltf` plus its
  external `.bin`/textures) now renders correctly, where before it failed
  silently (#497/#498/#507/#508, #486). Blender stays as an automatic fallback.

### Fixes

- **Federation-path query bug.** A metadata-adapter query referenced a
  nonexistent column (`owning_team_id` instead of the real `team_id`), so that
  path errored on every call. Pre-existing since ≤v0.5.2 and invisible to
  standard CI (which doesn't run federation); caught by the federation nightly
  and fixed before this release (#538).

- **CI reliability.** A large hardening pass on the test suite — shared-auth
  setup resilience, worker-isolation races, and timeout tuning — so a green run
  genuinely means green, not retry-masked (#485, #481, #505, #527, #535).

## [v0.5.2] — 2026-07-21

A content-visibility capability so read-only viewers (the public demo) can see
their whole catalogue.

- **`content.read.all` capability (#474).** A content-plane-only read cap,
  honored solely in `visibility.CanReadContent` alongside the `system.admin`
  wildcard — it grants asset-byte reads at every sensitivity tier and nothing
  else (no admin surfaces, no writes; it is not a wildcard). This lets a
  read-only role (e.g. the demo viewer) see `team`/`restricted` content that
  would otherwise return blank "Preview unavailable" tiles, without exposing
  any administrative surface. Migration `00014` defines the cap; granting it to
  a role is a deploy-side provisioning step (ADR 0060).

## [v0.5.1] — 2026-07-21

Promoted all of `dev` since v0.5.0 — the foundation work below (audit
retention/export, scheduled actions) plus two demo-surfaced fixes and a
visibility-consolidation batch. A patch version number, a substantial release.

### Operator-facing changes

- **Audit-log retention and export.** The audit log now has a retention
  policy — configurable per event category (a default of 7 years, with
  shorter or longer holds per category), a legal-hold flag that exempts
  individual events from purge, and a nightly enforcement pass. A GDPR
  erasure request anonymises a user across the log — the events are
  kept, the person is replaced by a `deleted-user` placeholder — so the
  trail survives without the personal data. And the whole log can be
  exported as CSV or NDJSON over a date range, streamed so exports of
  millions of rows don't exhaust memory; IP addresses are withheld from
  the export for callers who can't see them in the live view.

- **Scheduled actions.** Operators (and, later, the privacy, commerce and
  audit-retention features) can now schedule a change to run at a future
  time — change an asset's sensitivity, soft-delete, change state, or
  notify — and cancel it before it fires. Each action executes atomically
  with its audit entry, so it either fully happens and is logged or fully
  does not; a failure is recorded rather than half-applied. This is the
  generic engine (ADR 0020); the asset-gating features that use it —
  blur, reveal, timed embargo lift — land in later sprints.

### User-facing changes

- **Shareable, reloadable asset pages.** Assets now have a real
  `/assets/[id]` page, so a link to an asset opens, reloads, and shares
  correctly. Before this, clicking an asset inside a collection
  dead-ended on a "Not found" page — the tile linked to a route that
  never existed (#475). A build-time link-integrity check now guards
  against dead internal links (ADR 0068).

- **3D previews work on published builds again.** Turntable thumbnails
  for 3D models (glTF / OBJ / FBX and more) had silently stopped
  generating on released images — the published image shipped without
  the renderer — so every 3D asset showed no preview (#470). Fixed for
  amd64, with a build-and-render smoke so it can't regress unnoticed.

### Fixes

- Content-visibility hardening: soft-deleted collections no longer
  appear to signed-in non-owners, and the IIIF image path enforces the
  same visibility rule as the browse grid (#451, #460), plus audit and
  admin-gating cleanups (#458, #431).

## [v0.5.0] — 2026-07-20 — Public mode: anonymous browsing

Content is now reachable without an account, on an operator's terms. The
visibility model got a single enforcement point, sensitivity moved to the
content plane, and opening the surface surfaced (and closed) three
pre-existing access holes in the foundation it was built on.

### Operator-facing changes

- **A `public` visibility tier now exists** for collections and posts, and
  anonymous callers have a defined, enforced view of content: published,
  public, ready assets and public collections/posts only. Content
  visibility is decided in exactly one place — the visibility predicate —
  which every read path splices in (ADR 0063).
  Authenticated behaviour is deliberately unchanged. An authenticated
  caller still *sees* assets of every sensitivity in listings — that is
  intended, not a gap: sensitivity gates the bytes, never the rows, so
  restricted material stays listed as a locked item rather than
  vanishing (ADR 0020 via ADR 0064).
- **Asset browse now goes through that same predicate.** The browse query was
  sqlc-generated static SQL, which cannot accept a runtime fragment — it was
  the one read path visibility could not reach. Converted to hand-built SQL and
  gated. The superadmin-only `include_deleted` flag waives the soft-delete
  check **and only that** — publication status, sensitivity and processing
  state still apply, so the flag cannot drift into meaning "skip authorization".

- **Asset sensitivity is now enforced when serving files.** Previously any
  authenticated caller could download any asset's bytes — including `draft`
  and `restricted` material — because the byte-streaming endpoints checked
  only that a caller was signed in. Sensitivity now gates **content**: `team`
  assets require team membership, and `restricted`/`embargo` are limited to
  the owner and system administrators. Listing is deliberately unchanged —
  restricted assets remain visible as locked items rather than vanishing
  (ADR 0064, following ADR 0020). Denials return 404 rather than 403 so a
  response cannot be used to confirm that a restricted asset exists.

- **Two remaining copies of the visibility rule were removed, and a
  latent IIIF gap was found in the process.** Reverse-image search
  carried its own hand-written "anonymous sees public only" filter; it
  now uses the same visibility predicate as every other read path,
  which also correctly hides draft and still-processing assets that the
  old copy let through. The IIIF manifest layer keeps its own
  sensitivity gate — investigation confirmed it is not a duplicate but
  the *only* thing refusing a restricted asset's manifest to an
  anonymous caller, and a misleading code comment that invited its
  removal was corrected.

- **Audit-log IP addresses are now gated behind their own capability.**
  A read-only auditor could previously see the IP of every actor in the
  log — personal data that identifies people and approximates their
  location — because it rode along with the ordinary
  `system.audit.read` view. Seeing *what happened* and seeing *from
  where* are now separate grants: `system.audit.read` returns the log
  without IPs, and a dedicated `system.audit.pii.read` is required to
  see them. The address is withheld at the API, not merely hidden in
  the UI.

- **Access requests can no longer name a capability that doesn't exist.**
  `requested_capability` on an asset-access request was free text stored
  verbatim, in a field that feeds an authorisation decision — so a
  requester could put anything at all in it. It is now constrained to
  the real capability registry by a foreign key, and a request naming an
  unknown capability is rejected with a clear 400 instead of failing
  deeper in. Deleting a capability that still has outstanding requests
  now fails loudly rather than silently discarding the record of who
  asked for what.
  This narrows the field rather than fully securing it: a request can
  still name a *real* capability the requester shouldn't be able to ask
  for. Which capabilities are legitimately requestable is decided with
  the access-grant flow, which remains deliberately unbuilt.

- **A logged-out visitor now has something to look at.** Curated
  content can be featured for a public audience, and the front page
  renders it. Featuring is now a placement rather than a flag on the
  thing featured — the same collection can be featured publicly and
  internally at once, with its own ordering in each, and an individual
  asset can be featured without wrapping it in a collection.
  Two separate featured mechanisms had grown up side by side; there is
  now one. Featuring never widens access: a featured item renders only
  if the viewer could already see it, so publishing the rail does not
  publish the library.

- **Public browsing is now an operator choice, and it is off by
  default.** Anonymous access had no switch: any instance running this
  code served its public content to the internet whether the operator
  wanted that or not, and an existing install would have had it turned
  on by an upgrade. There is now a setting for it, enforced at the API
  rather than by hiding pages — turning it off means anonymous requests
  are refused, not merely unlinked. A fresh install starts private, and
  first-boot, login and SSO keep working with it off.

- **Signing in no longer hid public collections, and logged-out
  visitors could no longer see private ones.** Two visibility defects
  surfaced while opening anonymous access, both now fixed. An
  authenticated user got "not found" on a public collection they did
  not own — signing in *removed* access, and an administrator saw less
  than a logged-out stranger. Separately, the collection **list**
  endpoint applied no visibility rule at all, so an anonymous request
  returned every collection in the system, private ones included, with
  their names. Listing now goes through the same single visibility
  decision as every other read path.

- **A collection's contents are now visible to logged-out visitors —
  and were previously readable by any signed-in account.** Listing what
  is inside a collection applied no visibility check at all: any
  authenticated caller could enumerate the full contents of any
  collection by id, including collections they had no access to, and the
  response carried titles, types and publication status for draft
  material. The endpoint now checks the caller may see the collection,
  and filters the contents themselves — so a public collection shows
  only its public items to an anonymous visitor, while its owner still
  sees everything. Public collection pages render their contents rather
  than appearing empty.

- **Browsing without an account now works.** Listing assets and
  collections, and opening a single asset or collection, no longer
  require a signed-in caller: `GET /assets`, `GET /assets/{id}`,
  `GET /collections` and `GET /collections/{id}` serve anonymous
  requests, with the visibility predicate deciding what comes back —
  published, public, ready content only. Every write path still
  requires authentication.
  **This also closed a pre-existing hole**, which is the more important
  half: the two detail endpoints previously checked only that *some*
  caller was signed in and then fetched by id, so any authenticated
  account could read any asset or collection — including another
  user's private collection — simply by knowing its id. Both now run a
  real visibility check, and a denial returns 404 rather than 403 so a
  response cannot confirm that a hidden item exists.
  One consequence to expect: a public collection's *contents* are not
  yet anonymous, so a logged-out collection page shows its title and an
  empty body until that lands separately.

- **Anonymous visitors can now load public images.** The byte-streaming
  endpoints previously required a signed-in caller before anything else
  ran; they now defer to the same content check, which admits anonymous
  callers to `public`-tier assets and nothing else. `team`, `restricted`
  and `embargo` bytes remain unreachable without an account, across
  every byte-serving path (originals, derivatives, HLS segments and
  archive entries). This is the first surface where an anonymous request
  receives real content rather than metadata — the metadata endpoints
  are still authenticated and land separately.

### Infrastructure / housekeeping

- **The site now rebuilds from a signal that can fail.** When docs
  this repo owns change, the marketing site was rebuilt by firing a
  Cloudflare deploy hook — a bare POST that reports success for having
  been sent, not for a build that worked. Nineteen production deploys
  failed over twenty-four hours behind that signal with nothing to show
  it. The trigger now dispatches to the site repository instead,
  carrying the exact commit that changed so a rapid second push cannot
  cause the wrong content to be built, and a rejected credential fails
  loudly rather than skipping silently.


- `app/schema.sql` refreshed from a cleanly migrated database. The
  committed copy had drifted in **column order** — Postgres physical
  order is creation order, so columns added by later migrations land at
  the tail, and the stale file described an order the migrations never
  produce. That silently changed which Go types sqlc generated. Query
  column lists were realigned with the real schema; pg_dump's
  `\restrict`/`\unrestrict` markers are stripped so the file is
  byte-reproducible.
- Version files corrected to 0.4.0 (they had been left at 0.3.1).

## [v0.4.0] — 2026-07-18

Operator visibility: the async pipeline and the storage layer are now
observable and manageable from the admin surface. No-spec-impact.

### Operator-facing changes

- **Jobs admin.** The whole async pipeline (derivatives, previews, AI
  tagging, federation outbox) runs on the job queue, and until now it could
  only be inspected with `psql`. New surfaces, read-gated on
  `system.jobs.read` so a read-only operator can watch without holding
  `system.admin`: **queue** (jobs by status/type with age and priority),
  **workers** (active workers, lease state, stale-lease flag), **live**
  (status counts), **failed** (with `last_error`), **kinds** (per-type
  concurrency), **schedules** (future-dated work). Requeue, cancel, and
  concurrency edits require `system.admin`; a job that is currently running
  is never touched by either action.
- **Storage admin.** **Usage** (deduplicated bytes on disk, originals vs
  derivatives, breakdowns by content type and backend) and **variants**
  (per-family inventory), read-gated on the new `system.storage.read`.
- **Storage integrity sweeps.** `orphan_scan` reconciles the object store
  against the database in both directions; `checksum_verify` re-hashes
  stored bytes against the content-addressed key. Both run as batched,
  resumable job kinds, so they appear in the jobs queue like any other work,
  and both report into an admin surface. Findings are **advisory** and
  record scan time; no destructive cleanup ships in this release.
- **About reports the real version.** The page previously showed a
  hard-coded placeholder. It now reads a new anonymous `GET /build-info`
  endpoint serving the version baked in at build time. The displayed licence
  was also corrected to AGPL-3.0-only, matching the repository.
- **Help is visible to read-only operators.** Documentation, shortcuts,
  about, release notes, and support are now explicitly public admin tiles
  rather than implicitly superuser-only, and appear identically on desktop
  and mobile (ADR 0061).

### Infrastructure / housekeeping

- Storage backends gained an ordered, cursor-resumable `List` (ADR 0062).
  Filesystem walk order is not lexicographic over the key space, so the fs
  backend prunes and sorts to honour the contract; a shared contract test
  enforces it for every backend.
- Dependabot grouped per ecosystem into minor-and-patch versus majors with a
  lower open-PR limit, so routine bumps stay auto-mergeable and a batch no
  longer starves the self-hosted runners ahead of a release.
- Pre-checkout stale-`.git`-lock sweep on every self-hosted job, fixing
  intermittent checkout failures caused by cancelled mid-fetch runs.

## [v0.3.1] — 2026-07-17

Admin read-cap UI + foundation cleanup. No-spec-impact.

### Operator-facing changes

- **Admin UI for read-cap holders.** The frontend half of v0.3.0's read
  capabilities: the admin menu + route guard now gate **per-tile on the
  capability each surface enforces**, so a read-only role (without
  `system.admin`) sees and can browse the admin sections its caps permit —
  the admin menu lights up on the public demo. Backend still enforces every
  write.

### Infrastructure / housekeeping

- Repo-wide `gofmt` normalization + a `gofmt -l` CI gate.
- `make release` target codifying the release prep (version bump, openapi
  regen, drift check, open the promotion PR) — does not tag or toggle
  protection.
- Dependabot `github-actions` group split (routine bumps auto-merge; majors
  gated); steel secondary token wired into the Alert info tone.
- CHANGELOG + roadmap reconciled to current (they had drifted two releases
  behind).

## [v0.3.0] — 2026-07-17

Derivatives, read-only admin, responsive UI. No-spec-impact.

### Operator-facing changes

- **Media derivatives generated on seed/upload.** `aa seed` (and the
  upload path) now produce `col`/`hires`/`screen` thumbnails plus
  `sprites.jpg` video hover-scrub sheets — the browse grid renders real
  thumbnails instead of 404ing, and videos get a slideshow preview.
- **Read-only admin access.** A role can hold `*.read` admin
  capabilities and browse the admin surface **without** the
  `system.admin` superuser cap — six previously superuser-only surfaces
  (activities, featured, license, metadata-extraction, federation,
  requests) now render read-only, and the admin menu + route guard show
  each section per the capability its handler enforces. Backend enforces
  every write regardless.
- **Responsive + accessible UI.** Browse + navbar are fluid from a 390px
  phone to a 3840px / 32:9 ultrawide — an `auto-fill` grid where size is
  the lever and column count is the outcome (no breakpoint cliffs), an
  Instagram-style single-column `feed` view, hide-on-scroll chrome, and
  WCAG 2.2 AA target sizing on coarse pointers. Desktop layout unchanged.
- **Featured content curation** is seeded, so the admin Featured rail and
  the public collections featured tab both show content on a fresh seed.
- **Operator-bug fixes.** `PATCH /admin/system/site` now merges instead
  of blanking omitted fields (was: updating base_url wiped the site
  name); unroutable file extensions no longer mint guaranteed-terminal
  preview jobs; the nightly `ref` dispatch footgun is closed.

### Infrastructure

- CI/nightly stability arc — per-run compose isolation + resource caps,
  and five stacked shared-daemon/host causes fixed; the federation
  nightly is green for the first time since 2026-06-21. Repo-wide `gofmt`
  normalization + a `gofmt -l` CI gate.

## [v0.2.0] — 2026-07-16 — Admin surface unlock + public demo

Post-v0.1.2 incremental work. No-spec-impact.

### Operator-facing changes

- **Admin tiles unlocked (Tier 1–2).** The admin surface is now fully
  navigable: audit-log viewer (`/admin/audit`), per-user active
  sessions + capability grants/revokes, resource requests, **trash**
  with soft-delete restore across assets/posts/collections, system
  log, and an **API explorer served from the Go binary**
  (`/api/v1/openapi.json`, replacing the external-spec fetch).
- **`AA_DEMO_MODE`.** Env-gated demo mode — a `demo`/`demo` credential
  hint + fill button on the sign-in screen and a read-only banner
  when signed in as the demo user. Off by default; zero footprint on
  real installs.
- **Public read-only demo** at `demo.artist-alley.org` — runs the
  release image behind a write-blocking nginx edge, seeded from the
  Layer-A dataset, and auto-redeploys on each release.

## [v0.1.2] — 2026-07-15

> Reconstructed from the `v0.1.1..v0.1.2` commit range — this release was
> tagged without CHANGELOG or GitHub release notes at the time.

Brand, polish, and dependency hygiene; no wire-format changes.

### User-facing changes

- **Burnt/Steel brand.** Repaletted to the burnt accent + steel secondary,
  wired through components; finalized the chevron mark and the configured
  site-name handling; enlarged the sign-in brand mark; added a `viewBox`
  to the favicon/logo SVGs so the browser-tab favicon renders.
- **API docs are cleaner.** A usable getting-started, clearer error
  documentation, and internal phase codes dropped from the published spec
  (the first pass of the ongoing scrub).
- **Install quickstart fixed** — corrected `AA_MASTER_KEY`, the image path,
  the cosign identity, and pgvector setup.

### Fixes

- **Per-type job concurrency caps** are now applied in the single-process
  worker pool.
- **Saved-search notifications** no longer hot-loop — reschedules are
  grid-aligned.

### Infrastructure / housekeeping

- Supply-chain forks retargeted from `mscrnt/*` to `Artist-Alley-Org`.
- `pdfjs-dist` upgraded to v6; dependency sweep clearing Dependabot alerts.
- Test suite isolated from the shared dev database (#291); CI prunes
  dangling images to stop a runner disk leak.
- Real-world IP scrubbed from published surfaces; ArchivePub stamped
  v1.0-final (spec-only).

## [v0.1.1] — 2026-07-13

> Reconstructed from the `v0.1.0..v0.1.1` commit range — tagged without
> notes at the time.

A patch release restoring media processing and clearing shipped-artifact
vulnerabilities.

### Fixes

- **In-process worker pool never claimed jobs** (nil `Types` + a gate
  guard), so media processing silently stalled after v0.1.0. Fixed (#279)
  — this is the reason v0.1.1 exists.
- **GHCR image owner casing** — the org rename broke edge + release image
  pushes; the owner is now lowercased (#280).

### Infrastructure / housekeeping

- Shipped-artifact vulnerabilities cleared (torch floor raised, `aa-clip`
  bumped, npm sweep) — all open Dependabot alerts closed.

## [v0.1.0] — 2026-07-11 — Encryption arc (Phase 1.22.I)

The full encrypted-federation arc (1.22.I-a through 1.22.I-i) is
shipped + dogfood-validated end-to-end. ArchivePub spec at
**v1.0-rc1** with Appendix A conformance test vectors locked.
Seven-day soak window through **2026-06-22**; v1.0 final ships
as a no-code spec-only commit if soak is clean (otherwise
v1.0-rc2 first).

### Operator-facing changes

- **New** `POST /account/security/rotate-federation-keys` —
  user self-rotation of the X25519 federation keypair. Previous
  key is retained for the configured grace window (default 30
  days) so in-flight envelopes still decrypt.
- **New** `POST /admin/federation/users/{ref}/rotate-keys` —
  operator-initiated rotation for compromised-key recovery.
  `rotated_by_user_ref` records the admin's `user.ref` so the
  audit feed distinguishes recovery from self-rotation.
- **New** `GET /admin/federation/key-health` — aggregate
  observability dashboard data: users without a keypair, remote
  actors missing encryption keys, peers without negotiated
  capabilities, retained keys near expiry. Drill-down rows for
  the first + last categories ride along.
- **Behavior** Federation activities for `restricted`-tier
  assets are now encrypted end-to-end via NaCl-box. Senders
  refuse to dispatch when the recipient peer hasn't negotiated
  the `nacl-box` capability OR the recipient's pubkey isn't
  cached locally.
- **Behavior** Receivers reject plaintext envelopes targeting
  `restricted`-tier assets with `reject_reason=encryption_required`
  + audit `federation.inbox.encryption_required_rejected`.
- **Behavior** Asset sensitivity is set at create time (default
  `public`) and consulted by both sender + receiver gates.
  Changing the tier post-create propagates to in-flight
  emissions automatically (intentional: simpler than copy-at-
  grant semantics; a follow-up phase can layer the alternate
  behavior on top if operator feedback demands).

### Wire-format additions

- `aa:encryptionPublicKey` block in actor profile JSON (v0.3).
- `supported_capabilities` field in peer handshake offer /
  confirm envelopes (v0.4).
- `encryption` block in envelope JSON — per-recipient NaCl-box
  ciphertext + sender/recipient key id+version + nonce (v0.5).
- New reject reasons: `decrypt_failed` (v0.6),
  `encryption_required` (active at v1.0-rc1).

### New conformance test vectors

Appendix A of the spec now lists the 8 active scenarios under
`scripts/dogfood/scenarios/` that any conformant ArchivePub
implementation MUST pass against a peer running the reference:

- `01-like-cross-instance` — wire signature + dispatch
- `05-restricted-asset-roundtrip` — receiver-side defense gate
- `06-wire-dispatch` — outbox dispatcher + sub-1s p99
- `07-encryption-key-distribution` — actor profile + remote-actor cache
- `08-capability-negotiation` — handshake intersection
- `09-outbox-encryption-sender-side` — NaCl-box envelope shape
- `11-refusal-flip` — sensitivity-driven sender refusal
- `12-rotation-lifecycle` — rotation + sweeper + admin observability

Scenarios 02, 03, 04 remain outline scripts pending product
wiring (collection share UI, cascade observability).

### Migrations

| # | Schema change | Phase |
|---|---|---|
| 00007 | `federation_user_keys` table — X25519 keypair storage with `is_current` partial unique + multi-version retention | 1.22.I-b |
| 00008 | `federation_remote_actors.encryption_public_key` columns | 1.22.I-c |
| 00009 | `federation_peers.capabilities` + `capabilities_negotiated_at` | 1.22.I-d |
| 00010 | `federation_outbox.was_encrypted` + sender/recipient key version observability | 1.22.I-e |
| 00011 | `federation_inbox.was_encrypted` + `decrypted_with_key_version` | 1.22.I-f |
| 00012 | `federation_outbox.refused_reason` + `status='refused'` admission | 1.22.I-g |
| 00013 | `federation_user_keys.rotated_at` + `rotated_by_user_ref` + `system_config.federation.user_keys.retained_until_days` | 1.22.I-h |
| 00014 | `assets.sensitivity` (tier vocabulary + partial index on restricted/embargo) | 1.22.I-i |

### Backend admin observability

- 3 new audit events: `federation.user.key_rotated`,
  `federation.user.key_retained_expired`,
  `federation.inbox.encryption_required_rejected`.
- Background `userkeys.Sweeper` goroutine — ticks every hour
  with a boot-time first sweep covering downtime expirations;
  emits one audit per non-zero reap (quiet steady state).
- Receiver-side dispatcher stage-3.5 — gates plaintext envelopes
  against the target object's sensitivity tier via the
  `SensitivityLookup` callback (currently resolves `asset`-kind
  objects; other kinds pass through pending their own
  sensitivity columns).

### Out of scope / deferred

- Per-peer policy overrides ("always encrypt to peer X")
- Cross-instance key revocation broadcasts
- Hardware-token / HSM integration
- Algorithm migration mechanics (X25519 → P256 / PQ)
- `federation_shares.sensitivity` copy-at-grant semantics
  (asset-axis sensitivity is the single source of truth at v1.0-rc1)
