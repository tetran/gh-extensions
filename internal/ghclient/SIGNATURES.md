# API surface signatures (Phase 0 verification record)

> Phase 0 の probe で確定した go-gh の API surface と取得経路。実装中はここを参照、再検証が必要なら `probe/main.go` を直接 `go run ./probe/` で再実行可能（plan §4 Phase 0 では「Phase 1 開始前に削除」としていたが、再検証性を優先して保持）。

## 確認日付

- 2026-05-06T18:17:33+0900

## go-gh version pin

- `github.com/cli/go-gh/v2 v2.13.0`（最新 release tag、2025-11-04 release）
- Go toolchain: 1.25.0+ 必須（go.mod の `go` ディレクティブ）

## `api.NewRESTClient`

```go
import "github.com/cli/go-gh/v2/pkg/api"

client, err := api.NewRESTClient(api.ClientOptions{})
// メソッド:
//   client.Get(path string, resp interface{}) error
//   client.Post(path string, body io.Reader, resp interface{}) error
//   client.Do(method string, path string, body io.Reader, response interface{}) error
//   ... etc.
```

- `ClientOptions` の主要フィールド: `Host`, `AuthToken`, `Headers`, `Timeout`, `EnableCache`, `CacheTTL`, `CacheDir`
- 空 `ClientOptions{}` でも default で動作（`gh auth` の token と current host を継承）
- path は `/` なし起点（`"user"`, `"repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks"`）

## `repository.Current`

```go
import "github.com/cli/go-gh/v2/pkg/repository"

type Repository struct {
    Host  string
    Name  string
    Owner string
}

repo, err := repository.Current()  // returns Repository (struct, not 3-tuple)
```

- `Host` フィールドが含まれるので **GHES 互換性のため落とさず保持** する
- cwd の git remote から判定。`internal/ghclient.CurrentRepo()` は `Repository` を素通し

## `gh.Exec` (top-level package)

```go
import gh "github.com/cli/go-gh/v2"

stdout, stderr, err := gh.Exec("pr", "view", "13281", "--repo", "cli/cli", "--json", "...")
// stdout, stderr は bytes.Buffer
```

- `internal/ghclient.RunGhJSON(args ...string) ([]byte, error)` の薄いラッパとして使用
- A2 の `closingIssuesReferences` 取得は **このパス (a) を採用**（下記）

## A2 `closingIssuesReferences` 取得経路の確定

**採用: (a) `gh pr view --json closingIssuesReferences,...` を `gh.Exec` 経由**

probe 結果:

```
=== Probe 2: gh pr view --json via gh.Exec ===
keys=[mergeCommit number state closingIssuesReferences headRefName]
mergeCommit.oid=6c470f60803784e1558b626022677c53dccb6016
closingIssuesReferences len=1
```

- cli/cli PR #13281 → Issue #13280 で確認、JSON shape はそのまま `map[string]any` / 構造体に unmarshal 可能
- `mergeCommit.oid` 形式（オブジェクト下に `oid` 字段）で SHA を保持
- `closingIssuesReferences` は `[]{ id, number, repository: { id, name, owner: { id, login } }, url }` の配列
- (b) `gh api graphql` の自作クエリは fallback として保留（不要な複雑性を避ける）
- (c) raw REST は `closingIssuesReferences` を返さないため REJECTED 確定

## HEAD 解決経路の確定

**採用: `os/exec` で `git rev-parse HEAD`**

```go
out, err := exec.Command("git", "rev-parse", "HEAD").Output()
sha := strings.TrimSpace(string(out))
```

- go-gh には public な gitcontext API が無い（`internal/git` のみ、import 不可）
- clone 内で実行されている前提（`gh extension run` の通常使用）
- clone 外 / 空 repo / detached の場合は exit 128 → caller で「ref 解決失敗」として exitcode `UpstreamErr` (3) または明示エラーで返す

## 残課題（実装中に解決）

- `gh.Exec` 失敗時の stderr 内容を caller にどこまで露出するか（current 案: そのまま `internal/exitcode.UpstreamErr` でラップ + stderr プレフィックス）
- `--repo owner/name` フラグを各拡張で受けるか、`repository.Current()` 一択にするか → 各拡張で `--repo` optional flag を持たせる方針（Phase 1 で確定）
