# 実装計画: A 段 `gh` 拡張 4 件 (Go)

> **このドキュメントの位置付け**: `handoff-A-gh-extensions.md` および `~/.claude/skills/github-authoring/gh-tooling-candidates.md` §A1〜§A4 を踏まえた、本リポジトリ (`/Users/kkoichi/Developer/personal/gh-extensions`) での実装段取り。
> **機能仕様の source of truth は元ファイル**。本 doc は「何を、どの順で、どの足場で作るか」だけを扱う。期待出力フォーマットを書き直さないこと。

## Status

- 作成: 2026-05-06T17:29+0900
- 改訂: 2026-05-06T17:43+0900 — plan-reviewer 反映 v2（事実誤認 2 件 / `internal/` 構造誤認 / DoD 二段化 / Phase 0 切り出し / Q1 解決）
- 改訂: 2026-05-06T17:51+0900 — plan-reviewer 反映 v3（A3 exit code 確定 = regime 整合 (α) / HEAD 解決を Phase 0 表に追加 / `gh.Exec` 統一 / 軽微修正）
- 進捗: 計画 v3（着手前）
- 対象: A1 `gh protection-audit` / A2 `gh xref-verify` / A3 `gh ci-triage` / A4 `gh gist-content`

## 0. 解決済み事項 / 残 Open Questions

### 解決済み

#### Q1. 配布形態 → **モノレポ確定**（2026-05-06 ユーザー確認済）

ハンドオフ doc と候補 doc の間で drift（ハンドオフ：モノレポ / 候補 doc Last reviewed：4 リポジトリ独立）があったが、ユーザー判断で **モノレポ確定。repo を分ける予定なし**。

→ 帰結:
- §2 レイアウトの `internal/` 配置はモノレポ前提でそのまま採用可（Go の import 制限による「subtree split で分けられない」制約は当面無害）
- 候補 doc 側の drift は実装着手後の Last reviewed 更新で吸収する（ハンドオフ §Drift 対策に従う）

### Q2. `gh extension install` の配布形態（当面ローカルのみ）

ハンドオフ §配布で「当面ローカルのみ」と明言。よって `gh-extension-precompile` workflow のセットアップは **out of scope**。`gh extension install .` のローカル install のみ動けば良い。確認のみ。

### Q3. テスト戦略

ハンドオフに記載なし。提案：

- **unit**: `gh api` の JSON レスポンスを fixture で固定し、純粋関数（diff ロジック / 判定ロジック）を Go testing で検証
- **integration**: 既存の公開 repo（例: `cli/cli`）に対して read-only コマンドを叩き、exit code と先頭数行をスナップショットでアサート
- **CI**: 当面なし（ローカルのみ配布のため）。最低限 `go test ./...` がパスすればよい

## 1. Pre-flight verification（Phase 0 で実施）

ハンドオフ §決定事項で **「実装着手時に installed version で verbatim 確認すること」** が明示されている。以下を Phase 0 で確定する（独立フェーズ化の理由は §4 Phase 0 参照）:

| 確認項目 | 方法 | 用途 |
|---|---|---|
| `github.com/cli/go-gh/v2/pkg/api.RESTClient` のシグネチャ | `go doc github.com/cli/go-gh/v2/pkg/api.RESTClient` | A1〜A4 全部の `gh api` 呼び出し |
| `api.NewRESTClient(opts)` の opts 型 | `go doc` 同上 | client 初期化 |
| `repository.Current()` の返り値型 | `go doc github.com/cli/go-gh/v2/pkg/repository.Current` | repo context 解決。**返りは `Repository` struct（`Host` / `Owner` / `Name`）であり `(owner, name, error)` ではない**（pkg.go.dev で 2026-05-06 確認済）。`Host` を保持して GHES 互換性を確保 |
| A2 の `closingIssuesReferences` 取得経路（**3 候補から選択**） | 下表参照 | A2 PR↔Issue 双方向検証 |
| `gh pr checks --json` の Go 等価 | go-gh README / `gh api` direct fallback | A3 で失敗 check 取得 |
| HEAD 解決経路 | `os/exec` で `git rev-parse HEAD` vs go-gh / 他 SDK の gitcontext API を 1 本ずつ試す | A1 の `--ref` default 解決 |
| 最新 release tag | `gh release list -R cli/go-gh -L 3` | `go.mod` に pin する version |

### A2 の `closingIssuesReferences` 取得経路（3 候補）

REST `GET /repos/{owner}/{repo}/pulls/{pull_number}` は `closingIssuesReferences` を **返さない**（docs.github.com/en/rest/pulls/pulls で 2026-05-06 確認済）。経路は以下から選ぶ:

| 候補 | 概要 | 評価 |
|---|---|---|
| **(a) `gh pr view <PR> --json closingIssuesReferences,...` を go-gh の command runner 経由で実行** | gh CLI 内部で GraphQL を投げてくれるラッパ | **default 採用候補**。candidates §A2 step 1 と verbatim 一致。最少コード |
| (b) `gh api graphql -F query=...` を go-gh から直接 | GraphQL 自作 | (a) の出力 shape が扱いにくい場合の fallback |
| (c) raw REST | 不可 | **REJECTED**: closingIssuesReferences が response に含まれない |

→ Phase 0 で (a) を実コード片で確認 → 動けば採用、awkward なら (b) に倒す。

**verification 手順**: Phase 0 で小さな probe ファイル（`probe/main.go`）を作り、`api.NewRESTClient` 経路で 1 本叩く + `gh pr view --json` 経路で 1 本叩く、両方 `go run` で JSON が出ることを確認 → そこで分かったシグネチャを本実装に展開。probe は Phase 1 開始時に削除。

**verification 結果の persistence**: probe は捨てるが、確定したシグネチャ・選択した経路・確認日付は `internal/ghclient/SIGNATURES.md`（または `doc.go` ヘッダ）に転記する。CLAUDE.md「Verify before asserting」の趣旨は「あとで再検証可能なこと」であり、probe を消したら検証経路が失われるのは本末転倒。

CLAUDE.md「Verify before asserting > SDK method signatures」に該当する作業。training memory ベースで書き始めない。

## 2. レイアウト

モノレポ前提（Q1 で覆れば再構成）。

```
gh-extensions/
├── go.mod                          # module github.com/<user>/gh-extensions
├── go.sum
├── README.md                       # 4 拡張の install 手順 + 一行用途
├── implementation-plan.md          # 本 doc
├── handoff-A-gh-extensions.md      # 既存
│
├── cmd/
│   ├── gh-gist-content/main.go     # A4
│   ├── gh-xref-verify/main.go      # A2
│   ├── gh-protection-audit/main.go # A1
│   └── gh-ci-triage/main.go        # A3
│
├── internal/
│   ├── ghclient/                   # go-gh ラッパ（NewRESTClient + repo 解決）
│   ├── exitcode/                   # 0/1/2/3 規約の定数
│   └── output/                     # human-readable formatter（共通テンプレ）
│
└── testdata/                       # API JSON fixture（拡張ごと subdir）
    ├── A1/
    ├── A2/
    ├── A3/
    └── A4/
```

**理由**:

- `cmd/<name>/main.go` は Go 標準の multi-binary 慣習。`go install ./cmd/gh-gist-content` で個別 build 可能
- `internal/` で 4 拡張の重複（exit code 規約 / API client 初期化）を 1 箇所に集約
- `testdata/` は Go 標準（`go test` が自動で除外）

**`internal/` 配置の制約（覚え書き）**: Go の `internal/` は import 制限により、`cmd/gh-X/` 単独を `git subtree split` しても shared code を参照できずビルド不能になる。Q1 の決定（モノレポ確定 / repo 分割予定なし）によりこの制約は当面無害だが、将来 4 repo 分割を検討する場合は `internal/` を `pkg/` に昇格させる作業が前提となる。本計画では考慮しない。

## 3. 共通基盤（`internal/`）

### 3.1 `internal/exitcode`

ハンドオフ §共通実装パターン §exit code 規約を定数化:

```go
package exitcode

const (
    Success      = 0
    VerifyFailed = 1
    UsageError   = 2
    UpstreamErr  = 3
)
```

各拡張の `main` 末尾で `os.Exit(exitcode.X)`。途中の return では `error` を返し、`main` で type-switch して exit code に変換する。

### 3.2 `internal/ghclient`

go-gh の薄いラッパ。**Phase 0 (§1) の verification 結果を反映してから書く**。仮プロトタイプ:

```go
// pseudocode — シグネチャは Phase 0 で確定
package ghclient

func NewREST() (*api.RESTClient, error)              // api.NewRESTClient(opts) のラッパ
func CurrentRepo() (repository.Repository, error)    // 上流の Host/Owner/Name を全て保持
```

ポイント:

- `CurrentRepo()` は **`Host` を落とさない**こと。`repository.Repository` を素通しするか、3 値返り（`host, owner, name string`）にする。`(owner, name)` の 2 値に潰すと GHES 利用時に retrofit が必要になる
- A2 の PR/Issue cross-reference 取得は `gh pr view --json` 経路を使うため、`internal/ghclient` に `RunGhJSON(args ...string) ([]byte, error)` のような command-runner ラッパも置く（go-gh の `gh.Exec` 系 API を使用、シグネチャは Phase 0 確定）
- エラーは `internal/exitcode.UpstreamErr` に対応するセンチネルでラップ

### 3.3 `internal/output`

各拡張の human-readable 出力の共通要素（セクション見出し `=== ... ===`、`Last verified` 行）。各拡張の verbatim フォーマットは元ファイルが source of truth なので、`output` は薄いヘルパに留める（テンプレ化しすぎると元ファイルとの drift が増える）。

### 3.4 `--json` フラグ

ハンドオフ §共通実装パターンで「最初は人間可読出力のみで OK」。Phase 1 では実装しない。`flag.Bool("json", false, ...)` のスタブだけ置き、立てたら `fmt.Errorf("--json not yet implemented")` を返す形でも可（要相談）。

## 4. 実装フェーズ

ハンドオフ §実装順に従い軽い順。各拡張は **元ファイル §候補 AN を verbatim 参照** すること。

### DoD の共通形式（全 Phase に適用）

「verbatim 一致」は期待出力に timestamp / SHA / run ID 等の動的フィールドが混じるため厳格には適用不能。各 Phase の DoD は次の二段構造で書く:

- **構造一致**: 元ファイル §候補 AN 期待出力の固定文字列（セクション見出し `=== ... ===` / プレフィックス / 矢印 `←` / 警告文言テンプレ）が verbatim 一致
- **動的フィールド**: 日付・SHA・run ID・PR/Issue 番号は形式（regex / 桁数）でアサート
- **exit code**: 該当 Phase で発生し得る exit code 0/1/2/3 を各 1 ケースで確認
- **フラグ網羅**: 該当 Phase の全フラグの「指定あり」「default」両方を 1 ケースずつ
- `go test ./...` 緑

### Phase 0: Pre-flight probe（~1h）

§1 の verification 表を全部確定するための独立フェーズ。Phase 1 の足場確認に詰め込まないことで、A2 の経路選択（§1 の 3 候補 (a)/(b)/(c)）が「実装時に判断」に流れるのを防ぐ。

**何をやる**:

1. `go mod init github.com/<user>/gh-extensions` で module 初期化
2. `go get github.com/cli/go-gh/v2@latest`（最新 release tag を pin）
3. `probe/main.go` を作成し、以下 3 経路を `go run` で確認:
   - `api.NewRESTClient` で `GET /users/<self>` を叩いて JSON が返る
   - `gh pr view <既知 PR> --json closingIssuesReferences,mergeCommit,state` を go-gh の command runner（`gh.Exec`）から叩いて JSON が返る
   - HEAD 解決: `os/exec` で `git rev-parse HEAD` を叩く / go-gh の gitcontext API（存在すれば）を叩く、どちらが clone 内 / 外で安定するか比較
4. §1 verification 表の 7 行 + A2 経路選択 (a)/(b) の判定結果を `internal/ghclient/SIGNATURES.md` に転記:
   - 確認日付
   - go-gh version pin
   - `api.NewRESTClient` のシグネチャ
   - `repository.Current()` の返り値型（`Repository` struct のフィールド）
   - A2 経路の確定（(a) or (b)）と理由
   - HEAD 解決経路の確定（`git rev-parse` or gitcontext API）と理由
5. probe ファイルは Phase 1 開始前に削除（`SIGNATURES.md` が persist する）

**Definition of Done**:

- `go run probe/main.go` が 3 経路全部で JSON / SHA を出力（手で目視）
- `internal/ghclient/SIGNATURES.md` が存在し、§1 の 7 項目すべて埋まっている
- A2 の取得経路が (a)/(b) のいずれかに確定
- HEAD 解決経路が確定
- go.mod が cli/go-gh/v2 の release tag を明示 pin

### Phase 1: A4 `gh gist-content` — 最小足場確認（2〜3h）

**何をやる**:

1. `internal/exitcode` を最初に置く（4 拡張で参照される dependency なので先に）
2. `internal/ghclient` の最小実装（Phase 0 で確定したシグネチャベース）
3. `cmd/gh-gist-content/main.go`:
   - `flag` で `--no-sanity` 受ける
   - 引数 `<id> <filename>` をパース、足りなければ exit 2
   - `gh api gists/<id>` 相当で gist 取得
   - `.files["<filename>"].content` を取り出して stdout
   - `--no-sanity` でなければ先頭 1 行を sanity check（拡張子→期待 prefix のテーブル）
   - sanity NG なら stderr 警告 + exit 1
4. unit test: 拡張子→prefix 判定ロジックをテーブル駆動でテスト。`testdata/A4/gist.json` を fixture 化して content 抽出経路もテスト
5. `gh extension install .` で動作確認 → README に install 例

**Definition of Done**:

- 構造一致: §候補 A4「期待出力」の固定文字列（`gist-content: warning: expected '...' at start, got '...' (gist description leak?)` テンプレ）が verbatim 一致
- 動的フィールド: 該当なし（A4 は静的出力のみ）
- exit code: `0`（HTML 正常） / `1`（`weird.json` で sanity NG） / `2`（引数不足） / `3`（gist 不在で `gh api` が 404）の 4 ケース
- フラグ網羅: `--no-sanity` 指定あり（sanity skip）/ default（sanity 動作）の 2 ケース
- `go test ./...` 緑

### Phase 2: A2 `gh xref-verify` — JSON unmarshal パターン確立（3〜4h）

**何をやる**:

1. `cmd/gh-xref-verify/main.go`:
   - 引数 `<PR> <Issue> [<expected-sha>]` パース
   - **PR 情報取得**（Phase 0 で確定した経路）:
     - default: `gh pr view <PR> --json closingIssuesReferences,mergeCommit,state,number,headRefName` を go-gh の command runner 経由で実行（candidates §A2 step 1 verbatim）
     - fallback: `gh api graphql -F query=...` で同等フィールド
   - **Issue 情報取得**: `gh issue view <Issue> --json closedByPullRequestsReferences,state,number`（candidates §A2 step 2 verbatim）
   - 4 観点を順に検証（PR→Issue / Issue→PR / mergeCommit / states）
   - 全部 pass で stdout に元ファイル §A2「期待出力」の verbatim citation block + exit 0
   - どれか fail で fail した観点を stderr + exit 1
2. owner/repo は `repository.Current()` から取る（cwd が clone の中ならそれを使う、外なら `--repo` フラグ要否を相談）
3. unit test: 4 観点の判定を fixture-driven で網羅。状態の組合せ表でケース漏れ防止

**Definition of Done**:

- 構造一致: §候補 A2「期待出力（成功時）」の `✓ PR #N closes Issue #M (bidirectional)` / `✓ Merge commit: <sha>... (matches)` / `✓ States: PR=MERGED, Issue=CLOSED` / `Verified citation:` ブロックが verbatim 一致
- 動的フィールド: PR/Issue 番号 (`#\d+`)・merge commit SHA (`[0-9a-f]{7,40}`)・確認日付 (`\d{4}-\d{2}-\d{2}`) を regex でアサート
- exit code: `0`（4 観点 pass） / `1`（unidirectional ref / SHA 不一致 / 状態不整合 のいずれか） / `2`（引数不足） / `3`（PR/Issue 不在で API 404）
- フラグ網羅: `<expected-sha>` 指定あり（SHA 検証実施） / 省略（SHA 検証スキップ、§A2 仕様通り）
- `go test ./...` 緑

**注意点**:

- candidates §A2 と verbatim 一致させる：PR 情報の取得経路が `gh pr view --json` であることは仕様側に書かれているので、計画でこの形を崩さない
- `<expected-sha>` 省略時は SHA 検証スキップ（元仕様通り）

### Phase 3: A1 `gh protection-audit` — 双方向 diff + heuristic（4〜5h）

**何をやる**:

1. `cmd/gh-protection-audit/main.go`:
   - `--branch` (default `main`) / `--ref` (default `HEAD`) フラグ
   - `gh api repos/{owner}/{repo}/branches/<branch>/protection/required_status_checks` で `contexts[]` 取得
   - `gh api repos/{owner}/{repo}/commits/<ref>/check-runs` で `check_runs[].name` 取得
   - 双方向 set diff
   - 各 missing context に対して heuristic を当てる:
     - 名前に `#` を含む → `'#' truncation in source job name` 警告
     - run 側に `name (xxx)` 形式（matrix display）の near-match があるかを正規表現で見つける → `matrix job — actual was '...' / '...'` 警告
   - 元ファイル §A1「期待出力」の verbatim フォーマットで stdout
   - missing がゼロなら exit 0、あれば exit 1
2. **HEAD の解決**: `git rev-parse HEAD` を `os/exec` で取るか go-gh で gitcontext 経由か。pre-flight で決定
3. unit test: heuristic（`#` truncation / matrix display detection）を fixture でテスト。`contexts` と `check-runs` の各種食い違いパターンを網羅

**Definition of Done**:

- 構造一致: §候補 A1「期待出力」の `=== Required contexts NOT matched by recent check-runs ===` / `=== Recent check-runs NOT in required contexts ===` / `=== Last verified ===` セクション + `← suspect: '#' truncation in source job name` / `← suspect: matrix job — actual was '...' / '...'` / `← informational only` の警告テンプレが verbatim 一致
- 動的フィールド: `Last verified` の timestamp（ISO 8601 with TZ offset）、matrix job の actual 名（`build (3.10)` 等の括弧内 token）を regex でアサート
- exit code: `0`（missing 0 件） / `1`（missing 1 件以上） / `2`（usage error） / `3`（branch 不在で API 404）
- フラグ網羅: `--branch` default (`main`) / 明示指定、`--ref` default (`HEAD`) / 明示指定 計 4 通りのうち代表 2 ケース
- 検出ロジック単体テスト: `#` truncation 入り fixture / matrix display 形式 fixture / 純粋な rename ケース（heuristic 不適用）の 3 ケース
- `go test ./...` 緑

### Phase 4: A3 `gh ci-triage` — 解析量最大（6〜8h）

**何をやる**:

1. `cmd/gh-ci-triage/main.go`:
   - 引数 `[<PR>]` / `--workflow` / `--samples 8` フラグ
   - PR 番号未指定なら現在 branch から推定（`gh pr view --json number` を `gh.Exec` 経由で実行、§3.2 + Phase 0 の commitment と整合）
   - `gh pr checks <PR> --json name,status,conclusion,workflow,...` で失敗 check 抽出（同じく `gh.Exec` 経由）
   - 各失敗 workflow について:
     - `gh run list --workflow=<name>.yml --branch=main --limit=<samples> --json conclusion`
     - 失敗率を `M/N` 形式で集計
   - **失敗テストファイル抽出**（最も重い部分）:
     - `gh run view <run-id> --log` を grep で `FAILED tests/...` のような pattern を探す
     - 抽出できたら `git log --oneline -5 -- <file>` でファイル最終更新の commit を確認
     - その commit が当該 PR 内の commit か否かを判定（`git rev-list <pr-base>..HEAD` と照合）
   - 判定ルール（元ファイル §A3 のロジック通り）:
     - main 失敗率 ≥ 1/5 かつ test 最終更新が PR 外 → `PRE-EXISTING FLAKY`
     - main 失敗率 = 0/N → `LIKELY REGRESSION`
     - その間 → グレー（理由つき）
   - 元ファイル §A3「期待出力」の verbatim フォーマット
2. **ログ解析は fail-pattern adapter table 構造**で書く（拡張可能性を確保）:
   - `internal/logparse/` に `type Adapter interface { Extract(log []byte) (testFile string, ok bool) }` を置く
   - 初期実装は `pytestAdapter` のみ登録（`FAILED tests/...::test_X` 形式を抽出）
   - 未マッチなら `unable to extract test file` を出して残りの判定（main 失敗率）は続行
   - 将来 Go (`--- FAIL: TestX`) / Jest (`✕`) を足すときは adapter 追加のみ
3. unit test: 判定ロジック（失敗率 + ファイル変更主体）をテーブル駆動で網羅。ログ grep は pytest fixture 1 本 + 未マッチ fixture 1 本

**Definition of Done**:

- 構造一致: §候補 A3「期待出力」の `PR #N CI failures:` / `workflow:` / `PR result:` / `main last N runs: M FAIL / K PASS  ← X% flaky` / `test file:` / `verdict: PRE-EXISTING FLAKY` or `LIKELY REGRESSION` / `evidence:` 行が verbatim 一致
- 動的フィールド: PR 番号、failure rate `\d+%`、commit SHA、ファイルパス、main 失敗率の `M FAIL / K PASS` を regex でアサート
- exit code: `0`（PR 成功 / `PRE-EXISTING FLAKY` / グレー判定） / `1`（**1 件以上の `LIKELY REGRESSION` 判定**、§6 で確定した regime 整合 (α)） / `2`（usage error） / `3`（PR 不在で API 404 等の上流障害）
- フラグ網羅: `<PR>` 指定あり / 省略（branch から推定）、`--workflow` 指定あり / default、`--samples 8` default / `--samples 4` / `--samples 16` の差し替え 2 ケース
- graceful degradation: pytest 以外の workflow（adapter 未登録）で `unable to extract test file` を出しつつ main 失敗率の集計は続行する 1 ケース
- `go test ./...` 緑

**スコープ制限の覚え書き**: pytest 以外の workflow サポートは adapter 追加で対応可能な構造とする。他の adapter（Go の `--- FAIL: TestX` / Jest の `✕` 等）の実装は本計画外（README に「現状 pytest 系のみ adapter 登録済」と明示）。

## 5. README（最後にまとめて書く）

各拡張の install / 一行用途 / exit code を 1 ファイルに集約。期待出力サンプルは元ファイルの §候補 AN へのリンクで済ませ、verbatim duplication しない（drift 源を増やさない）。

## 6. リスクと判断保留点

| 区分 | 内容 | 対応 |
|---|---|---|
| ~~仕様 drift~~ | ~~Q1（モノレポ vs 4 repo）~~ | **解決済**: モノレポ確定（2026-05-06） |
| API surface | A2 の `closingIssuesReferences` 取得経路は (a) `gh pr view --json` / (b) `gh api graphql` の 2 択（(c) raw REST は不可と確認済） | Phase 0 probe で (a) を試す → 動けば採用 |
| ログ解析 | A3 の test file 抽出は workflow 出力依存 | adapter table 構造、初期は pytest のみ登録、未マッチは graceful degradation |
| HEAD 解決 | A1 の `--ref` default を `git rev-parse HEAD` で取るか別経路か | Phase 0 で決定 |
| `--json` flag | 後付け予定だが Phase 1 では未実装 | スタブだけ置く or フラグ自体を後送り |
| A3 exit code | 元仕様で regression 判定時の exit code が未明示 | **regime 整合 (α) を採用**: `LIKELY REGRESSION` → exit 1、FLAKY / グレー / 解析正常 → 0、上流 API 障害 → 3。他 3 拡張（A1/A2/A4）と同じく `gh ci-triage <PR> && deploy` の chaining が成立する設計。情報用途で全部 0 にしたい場合は将来 `--quiet-verdict` flag で対応 |
| テスト戦略 | Q3（unit / integration / CI 方針） | 着手時にユーザー合意 |

## 7. Drift policy

- 本 doc は **計画フェーズの作業指示書** であり、実装中に元ファイル (`gh-tooling-candidates.md` §A1〜A4) や `handoff-A-gh-extensions.md` が書き換わったら本 doc を更新
- 実装フェーズに入った後は、各 `cmd/<name>/README.md`（あるいはモノレポ README）と元ファイルの「Last reviewed」が source of truth。本 doc は凍結
- 期待出力の verbatim フォーマットを本 doc に転記しない（元ファイルとの drift を増やさないため）

## 8. 着手手順サマリ

1. **ユーザー確認**: §0 Q3（テスト戦略）を確認 — Q1 はモノレポで解決済
2. **Phase 0**（~1h）: `go mod init` → `probe/main.go` で API surface 確定 → `internal/ghclient/SIGNATURES.md` に persist
3. `internal/exitcode` を Phase 1 冒頭に置く（4 拡張で参照される dependency なので、CLAUDE.md「依存先を先に作る」原則）
4. **Phase 1 (A4)** → **Phase 2 (A2)** → **Phase 3 (A1)** → **Phase 4 (A3)**
5. 各 Phase の DoD（構造一致 / 動的フィールド / exit code / フラグ網羅 / `go test`）を満たしてから次へ
6. 全 Phase 完了後に README とハンドオフ doc 末尾の進捗欄を更新（ハンドオフ §Drift 対策に従う）
