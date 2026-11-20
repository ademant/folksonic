---
name: add-endpoint
description: Checklist for adding or changing an HTTP endpoint in tinysonic/folksonic — a Subsonic /rest endpoint, an OpenSubsonic extension, a JSON /api route, or a refusal code. Use whenever the user asks for a new endpoint, route, API field or refusal/error code, or when a change in internal/server adds one. Covers registration, auth, the response envelope, docs and the tests that enforce them.
---

# Adding an endpoint

Endpoints are easy to register and easy to leave half-done: the code compiles, but the docs test or the auth rules fail later. Work through this list in order, and run `make check` at the end (see `ship-change`).

## 1. Pick the kind

| Kind | Registered in | Auth | Response |
|---|---|---|---|
| Subsonic `/rest/x` | `register("x", handler)` in `internal/server/server.go` (adds `/rest/x` and `/rest/x.view`, GET and POST) | `s.requireCaller` or `s.requireAuth` in the handler | Subsonic envelope: `writeOK(w, J{...})`, `writeError(w, code, msg)` |
| Raw Subsonic (audio, images) | `registerRaw` | as above | binary, not wrapped |
| `/api/...` JSON route | `mux.HandleFunc("GET /api/...", s.Handler)` in `server.go` | `s.requireUser` (any user) or `s.requireSession` (no API keys, no device proofs) | JSON: `writeJSON`, errors via `writeAPIError` |

A route without one of those auth calls is a bug unless it is deliberately public. Say so in the handler comment if it is public.

## 2. Write the handler

- Keep the Subsonic envelope exactly. Clients are sensitive to field names and shapes (CLAUDE.md).
- Mind Subsonic clients that send `GET` and `POST`, and `.view` suffixes: the registration already covers them.
- Errors you return need a Subsonic code or an HTTP status that is documented.
- For a new refusal code (a credential or a grant that is refused), add a `users.Code…` constant in `internal/users/tokens.go`, a message in `refusalMessages` in `internal/server/refusal.go`, and a line in the README's *Refusal codes* table. Log refusals with `s.logRefused`, not with ad-hoc log lines.

## 3. Advertise an extension (if it is one)

A vendor or OpenSubsonic extension goes into `baseOpenSubsonicExtensions` in `internal/server/opensubsonic.go`, with a comment naming the issue. Add a row to the extension table in `docs/opensubsonic-endpoints.md`, with its version and what it adds.

## 4. Docs, in the same change

- Subsonic endpoint: add it to `## Implemented` in `docs/opensubsonic-endpoints.md` (plain text for parameter words, see `ship-change`), and update the count in that heading and the README's *Supported endpoints* count.
- `/api` route: add a path entry to `docs/openapi.yaml`. `TestOpenAPIDocumentsEveryAPIRoute` fails without it. Include the auth (`bearerAuth` or `security: []` for public), request and response shapes, and the status codes.
- README: the section for the feature, and the *How auth works* list if the route accepts a new credential.

## 5. Tests

- Start from `newTestState(t)`, or `keyFixture(t)` for a state with an API key (it also returns a worker-scoped key and a full key for `bob`). `userIDOf(t, s, name)` gives an ID.
- Use `getWith(t, handler, path, key, inHeader)` for a request with a key, and `subsonicBody` and `errorCode` to read the Subsonic answer.
- Cover the auth rules, not just the happy path: an API key refused where a session is required (`session_required`, 403), a missing credential (401), and a wrong-user case if data is per user.
- Check the new route is reachable at both `/rest/x` and `/rest/x.view`, if it is a Subsonic endpoint.

## 6. Migrations

If the endpoint needs a table or a column, follow `internal/db/version.go`: add a numbered step to the `migrations` slice, make it idempotent (`CREATE TABLE IF NOT EXISTS`), and run the migration tests. Changing an existing `CREATE` statement alone does not reach existing databases.

## 7. Check

`make check`, then confirm in the diff that the doc files changed too: `git diff --stat docs/ README.md`. A new route with no docs change is incomplete.
