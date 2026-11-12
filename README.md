# prospabitus

コーディングエージェントが自らをふりかえるためのツール

## 使い始める

```sh
go build -o prospabitus ./cmd/prospabitus
# PATHへインストールする場合は go install ./cmd/prospabitus
# 以下はPATHに入れた場合の例。ローカルのビルドなら ./prospabitus
prospabitus doctor
prospabitus sync
prospabitus status
prospabitus sessions list
prospabitus search '日本語で'
prospabitus skill show
```

`doctor` は既定の探索先と実行環境を返します。`sync` は最初に既定のログディレクトリを登録し、以降は同じ取得先を使います。取得先を指定・変更する場合：

```sh
prospabitus --profile work sync \
  --source codex=/work/logs/codex \
  --source claude-code=/work/logs/claude
prospabitus --profile work sources
prospabitus --profile work sync
```

`--source` は毎回指定する必要がありません。明示すると、そのプロファイルの取得先一覧を置き換えます。複数ディレクトリを指定できます。保存先は `${XDG_DATA_HOME:-~/.local/share}/prospabitus/<profile>/archive.sqlite3`。`--data-dir` でルートを変更できます。グローバル引数はサブコマンドの前に置きます。個人・会社のデータは別プロファイル／別端末で管理してください。プロファイル自体はOSのアクセス制御ではありません。

## セッションを読む

```sh
prospabitus sessions show 'codex:<native-session-id>'
prospabitus events --session 'codex:<native-session-id>' --limit 20
prospabitus search '承認' --limit 20
prospabitus search '日本語' --agent claude-code --role user
prospabitus search '調査' --include-internal
prospabitus context 42 --radius 3
prospabitus event 42 --offset 4000
prospabitus changes --after 0
prospabitus events --revision 3
prospabitus revisions --session 'codex:<native-session-id>'
prospabitus export --revision 3 --output saved.jsonl
```

- 通常の出力は1つのJSON。`schema_version` と `ok` を含み、失敗は非ゼロ終了。help/versionは人間向けテキストです。
- 一覧は既定20件、最大100件。`next_cursor` があれば `--after` に渡します。通常のevents/search本文は4000文字まで。`event ID --offset N` で8000文字ずつ続きを読めます。`context` は同じrevisionの前後のイベントを返します。
- 通常検索・セッション一覧からは承認審査とsubagentを除外します。一般の委譲先subagentも含めて見る場合は `--include-internal`。直接のsession/revision取得とchangesはすべての記録を保持します。
- `search --agent codex|claude-code --role user|assistant|developer|tool` で絞れます。ツール・指示のrole検索には `--all` も指定します。
- 通常検索は有効な最新revisionのuser/assistantメッセージ。大文字小文字を区別するUnicode部分一致で、「承認」のような短い日本語も検索できます。ツール／指示は `--all` で対象にできます。userには自動挿入情報も含まれます。`origin=unknown` を人間由来と断定しないでください。
- `changes` は発言日時ではなく保存された順番のカーソルです。古い会話への追記も拾えます。ページの `cursor` までの各revisionを読んで処理してから、利用側でカーソルを保存してください。各revisionはその時点の**全体**であり、差分だけではありません。同じ過去の発言を重ねて数えないでください。カーソルは当該DBだけで有効です。
- `export` は保存済みのJSONL原本を復元します。既存ファイルを上書きしません。書きかけ末尾は含みません。
- 原本やツール結果には秘密情報も含まれ得ます。DBの新規作成時は0600、専用ディレクトリは0700です。ローカルの平文保管です。

## 取得元と対応範囲

| アダプター | 既定の探索先 | 形式・識別子 | 利用経路の判定 |
|---|---|---|---|
| codex | `$CODEX_HOME/sessions`, `$CODEX_HOME/archived_sessions`（既定 `~/.codex`） | JSONL / session_meta.id または session_id | originatorのDesktop表記、source=cli/exec。それ以外unknown |
| claude-code | `$CLAUDE_CONFIG_DIR/projects`（既定 `~/.claude`） | JSONL / sessionId。subagentsはファイル名も付与 | entrypoint=claude-desktop / cli。それ以外unknown |

CLI/GUIは同じアダプターを使います。ローカルに保存されたDesktopのコーディングセッションが対象です。クラウドにしかないセッション、通常のChatGPT/Claudeチャットは取得しません。元のログは変更しません。取得ルート自体がシンボリックリンクの場合は拒否し、ルート配下のリンクファイルやリンクディレクトリは走査しません。親パスのOSエイリアス（macOSの `/tmp` など）は利用できます。

この環境ではCodex CLI 0.167.0、Desktop 0.167.0-alpha.4.1、Claude Code CLI 2.1.302、Desktop 2.1.260の構造サンプルでパーサーを照合しました。Claude CLI標本にはassistantメッセージがなく、CLI応答は共通スキーマの合成fixtureで検証しています。バージョン一般の互換性を保証するものではありません。`testdata` は実ログの必要フィールドだけを取り出し、本文・ID・パス等を架空値に置換した構造fixtureです。

Codexはresponse_item.messageを正規化し、表示用item_completedの発言のうちミラーがないもの（確認質問・CLIレビュー指示等）も補完します。同じrole・本文／テキストブロック・記録済みturn IDで対応を取り、反復した依頼は回数を維持します。元の来歴属性が環境情報・heartbeatと示すものはcontext／notification、ClaudeのisMeta／isCompactSummaryもcontextとして通常検索から外し、`--all` で参照できます。Claudeはtextとtool_use/tool_resultを取り出します。分岐の親ID、圧縮、thinking、未知レコードなどは原本に残ります。Claudeの標準ログはUUIDのファイル名をIDに使い、再開直後にコピー履歴しか書かれていなくても同じIDを維持します。任意の名前で保存したexportは先頭の記録IDを使い続けます。コピーされた旧セッションの発言はcontextとして保存し、通常検索で二重に数えません。`--all` で読めます。コピー元のID一覧は `inherited_session_ids` に残しますが、UI上の直接の分岐親とは断定しません。

完全なUI再現、分岐選択、全イベント形式の検索用正規化は初期版の対象外です。添付ファイルの参照は残りますが、画像・音声などの実体はコピーしません。

## 保全と競合

`sync` はファイルを再走査します。改行で完結したJSONLのスナップショット、正規化イベント、active revision、変更カーソルを同じSQLiteトランザクションで保存します。SQLiteの書き込みロックで同時取り込みを直列化します。完全な行が不正JSONの場合はファイル単位で失敗し、そのファイルの新しい内容は保存しません。既存の保存内容は残ります。

同じエージェントの同じ元セッションIDを同じ系列として扱います。本文が同じ別メッセージを削除しません。元ログ削除は反映しません。同じ内容の再実行や古いprefixのコピーでは履歴を増やしません。追記は新revisionとして保存します。

内容が有効な原本のprefix拡張ではない場合、競合revisionを保全し、通常検索の原本は切り替えません。`sync` は `ok=false` とactive/conflict revisionを返します。`revisions` → `events --revision` / `export --revision` で双方を確認できます。同じ競合の再実行で保存量・changesは増えません。圧縮等による正当な書き換えも競合になります。自動解決は未実装です。

最小版では変更されたファイルの全体を各revisionに保存するため、長いセッションへの頻繁な同期では容量が増えます。大規模データ向けの差分チャンク化・FTS・保持期限はまだありません。欠損した既定ルートは `missing_roots`、未完末尾は `pending_files`、解析失敗・競合は `errors` に出ます。`sync` の成功だけで全製品の全履歴が揃ったとは判断しないでください。

## License

Copyright (c) 2026 Rokoucha

Released under the MIT license, see LICENSE.
