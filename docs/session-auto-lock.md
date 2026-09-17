# Session error auto-lock

`/session-errors` exposes a disabled-by-default policy with a default threshold of
3 (configurable from 1 to 10000). Only final HTTP 500 results count. SSE and
WebSocket requests use their terminal usage/error status, not their successful
HTTP handshake. Internal attempts do not count separately. Results are ordered
by completion within the existing user/platform/root-session identity; any
non-500 result resets the streak. Requests without a reliable session identity
cannot be locked by this feature.

Main requests and related background requests (including Guardian) share this
counter when they resolve to the same root. Once a final usage result is recorded,
later connection cancellation does not replace it. An actual final 499, or a
cancellation without a final usage result, still resets the streak.

API relay requests are excluded, including a relay selected from a mixed account
pool. An API key used to call a Codex account does not itself grant this exemption.

At the threshold the existing persistent session blacklist is updated with
`lock_source=automatic`. The UI labels it “自动锁定” and supports
`lock_state=auto_locked` in both statistics and blacklist views. Existing parent
and fork enforcement and exemptions remain in effect. In-flight requests are not
forcibly canceled. Administrators can unlock automatic entries normally.

Settings, streaks, and locks survive restart. Saving settings resets pending
streaks and invalidates older requests' policy snapshots. Disabling the policy
does not unlock existing entries. Manual unlock resets the streak and older
in-flight requests cannot immediately re-lock it. Historical errors are never
backfilled into streaks. Existing manual entries default to `lock_source=manual`.

Settings API (admin authentication required): GET / PUT
`/api/admin/session-errors/auto-lock` with `{ "enabled": false, "threshold": 3 }`.
The disabled path performs no counter database work; the enabled path uses
indexed per-session updates instead of scanning logs. Lock attribution queries
are limited to the current page. Persistence failure is logged and never changes
the already completed model response.
