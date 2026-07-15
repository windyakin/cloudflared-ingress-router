# cloudflared-ingress-router

Ingress リソースの annotation を読み取って、Cloudflare Tunnel (cloudflared) の公開設定と DNS レコードを自動管理する Kubernetes カスタムコントローラです。

HTTP / HTTPS のルーティングは既存の Traefik（などの Ingress Controller）に集約する構成を前提としており、cloudflared の各ホスト名のオリジンは既定で Traefik の Service を向きます。

```
インターネット → Cloudflare edge → cloudflared → Traefik → 各 Service
                      ▲
                      │ ①トンネル設定 PUT / ②DNS CNAME 作成
              cloudflared-ingress-router ← Ingress (annotation) を watch
```

## 動作の仕組み

- annotation `cloudflared-ingress-router.windyakin.net/enabled: "true"` が付いた Ingress の `spec.rules[].host` を公開ホスト名として収集します
- 全対象 Ingress を集約して 1 つのトンネル設定（ingress rules + 末尾の catch-all `http_status:404`）を組み立て、[Tunnel configurations API](https://developers.cloudflare.com/api/resources/zero_trust/subresources/tunnels/subresources/cloudflared/subresources/configurations/) に PUT します（remotely-managed tunnel）。**cloudflared の再起動なしで即時反映**されます
- 各ホスト名について、トンネルへ向く proxied CNAME レコード（`<hostname>` → `<tunnel-id>.cfargotunnel.com`）を作成します
- Ingress の削除・オプトアウト時は finalizer によって DNS レコードとトンネル設定から確実に除去されます。DNS レコードは comment フィールドの管理マーカーで所有権を判定するため、手動や他ツール（external-dns 等）が作ったレコードには触れません

## 前提条件

- トンネルが **remotely-managed** であること。cloudflared は `cloudflared tunnel run --token <TOKEN>` で起動してください
  - locally-managed（config.yaml + credentials.json）で運用中の場合は、ダッシュボードまたは API からトンネル設定を remote 管理に移行し、Deployment の起動引数を token 方式に変更する必要があります
  - なお `cloudflared tunnel run` は config ファイルのホットリロードを行わないため、ConfigMap マウント方式では設定変更を反映できません（本コントローラが API 方式を採る理由です）
- **トンネルは本コントローラ専有**になります。configurations API は全置換のため、ダッシュボード等でトンネルに加えた手動の Public Hostname 設定は定期リコンサイル（既定 10 分）で上書きされます
- Cloudflare API Token に以下の権限が必要です
  - Account > Cloudflare Tunnel > Edit
  - Zone > DNS > Edit
  - Zone > Zone > Read

## インストール

```sh
# Secret を作成
kubectl create namespace cloudflared-ingress-router
kubectl -n cloudflared-ingress-router create secret generic cloudflared-ingress-router \
  --from-literal=accountId=<CLOUDFLARE_ACCOUNT_ID> \
  --from-literal=tunnelId=<TUNNEL_ID> \
  --from-literal=apiToken=<API_TOKEN>

# マニフェストを適用（オリジン URL 等は config/deployment.yaml を環境に合わせて編集）
kubectl apply -k config/
```

## 使い方

Ingress はこれまでどおり Traefik（`ingressClassName: traefik`）に処理させたまま、annotation を追加するだけです。

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: myapp
  annotations:
    cloudflared-ingress-router.windyakin.net/enabled: "true"
spec:
  ingressClassName: traefik
  rules:
    - host: myapp.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: myapp
                port:
                  number: 80
```

これだけで `myapp.example.com` の CNAME レコードが作成され、トンネル経由（cloudflared → Traefik → myapp）で公開されます。

### Annotation リファレンス

prefix: `cloudflared-ingress-router.windyakin.net/`（`--annotation-prefix` で変更可能）

| annotation | 既定値 | 説明 |
|---|---|---|
| `enabled` | - | `"true"` で公開対象にする（オプトイン） |
| `origin-scheme` | `https` | cloudflared → Traefik の接続スキーム。`http` / `https` |
| `origin-service` | コントローラの既定オリジン URL | オリジンの完全上書き（例 `http://myapp.default.svc.cluster.local:8080`、Traefik を経由させない場合） |
| `no-tls-verify` | `false` | オリジン証明書の検証を無効化 |
| `origin-server-name` | 各ホスト名自身 | オリジン TLS の SNI / 検証名。既定でホスト名が渡るため、Traefik が SNI に応じた正しい証明書を返せます |
| `http-host-header` | (未設定) | オリジンへ送る Host ヘッダの上書き |
| `http2-origin` | `false` | オリジンへ HTTP/2 で接続 |
| `ca-pool` | (未設定) | オリジン検証用 CA バンドルの **cloudflared コンテナ内パス**。証明書自体は cloudflared Pod に Secret 等でマウントしておく必要があります |

- 公開ホスト名は annotation ではなく `spec.rules[].host` から取得します（Traefik のルーティング定義と共有）
- 同一ホスト名を複数の Ingress が公開しようとした場合、namespace/name の辞書順で先勝ちとなり、負けた側には Warning Event（`HostnameConflict`）が記録されます
- ワイルドカードホスト（`*.example.com`）も公開できますが、既定の `origin-server-name` は付与されません。オリジンが HTTPS の場合は `origin-server-name` を明示するか `no-tls-verify: "true"` を指定してください

## コントローラのフラグ

| flag | 既定値 | 説明 |
|---|---|---|
| `--account-id` | (必須) | Cloudflare アカウント ID |
| `--tunnel-id` | (必須) | 管理対象のトンネル ID |
| `--annotation-prefix` | `cloudflared-ingress-router.windyakin.net` | annotation のドメイン部 |
| `--origin-url-https` | `https://traefik.kube-system.svc.cluster.local:443` | `origin-scheme: https` 時の既定オリジン |
| `--origin-url-http` | `http://traefik.kube-system.svc.cluster.local:80` | `origin-scheme: http` 時の既定オリジン |
| `--resync-interval` | `10m` | ドリフト矯正のための定期リコンサイル間隔 |
| `--leader-elect` | `false` | リーダー選出の有効化 |

API トークンは環境変数 `CLOUDFLARE_API_TOKEN` で渡します。

## 既知の制約

- cloudflared のレプリカを増やした直後、新レプリカが設定を受け取るまでの数秒間 503 を返すことがあります（[cloudflared#1171](https://github.com/cloudflare/cloudflared/issues/1171)）
- ゾーン一覧はコントローラ起動時にキャッシュされます。Cloudflare アカウントにゾーンを追加した場合はコントローラを再起動してください
- 1 トンネルあたりの cloudflared 接続上限は 100（= 25 レプリカ）です

## 開発

```sh
make test          # go test
make build         # bin/cloudflared-ingress-router
make docker-build  # コンテナイメージのビルド
```
