;; caixa.lisp — the single source of truth for todoku-go's kind + ecosystem.
;;
;; Consumed by `pleme-doc-gen` for the SDLC pipeline (go.mod + flake.nix +
;; .pleme-io-release.toml + CI shims + nix module trio).
;; Re-emit the generated surface with:
;;   pleme-doc-gen caixa --source caixa.lisp --out . --force
;;
;; NOTE: the *.go sources, README.md, LICENSE, and CHANGELOG.md are AUTHORED and
;; are NOT regenerated. The flake.nix is the canonical hand-tuned Biblioteca
;; shape (substrate goLibraryFlakeBuilder, vendorHash omitted → spec-sourced;
;; the only external deps are golang.org/x/{net,time}, confined to the
;; todoku/h2 + todoku/budget leaf sub-packages per BOREALIS Law 6).

(defcaixa todoku-go
  :kind         :Biblioteca
  :ecosystem    :go

  :package      { :name        "todoku-go"
                  :version     "0.3.0"
                  :license     "MIT"
                  :description "pleme-io's standard outbound-HTTP client for Go — one Client + one RetryWithBackoff primitive, full-jitter backoff, Retry-After, idempotency, breaker, retry budget, transport tuning, service-reachability Probe, and single-flight/result-cache dedup middleware (届く)."
                  :module-path "github.com/pleme-io/todoku-go"
                  :repository  "https://github.com/pleme-io/todoku-go"
                  :homepage    "https://github.com/pleme-io/todoku-go"
                  :go-version  "1.25" }

  :supports     { :go ">=1.25" }

  :ci-config    { :bump    { :default-type "patch" }
                  :publish { :no-verify false } }

  :workflows    [ :auto-release ]
  :stacks       [ ]
  :depends-on   [ ]
  :exposes      [ :go-module ]
  :publish-to-git true)
