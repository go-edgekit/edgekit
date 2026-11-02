# AGENTS.md

- A wrapper of `http.ResponseWriter` MUST implement
  `Unwrap() http.ResponseWriter`.
- Flush and hijack through `http.ResponseController`. Do not assert
  `http.Flusher` or `http.Hijacker`.
- A wrapper that holds back the status code MUST NOT flush before it writes
  that status, because a flush commits an implicit `200`.
