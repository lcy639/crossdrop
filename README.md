# CrossDrop

自托管的跨设备文件中转站。用浏览器上传文件，再通过六位配对码、链接或二维码在另一台设备上下载。服务端使用 Go，前端使用 React 和 TypeScript。

- 多文件传输和分块上传。重新打开相同地址并选择相同文件，可接着上传已保存的分块。
- 合并后校验 SHA-256。下载支持 HTTP Range。
- 发送与接收权限分开，房间可主动关闭，过期文件自动清理。

## 本地启动

需要 Docker 和 Docker Compose。

```sh
docker compose up -d --build crossdrop
```

打开 [http://localhost:8787](http://localhost:8787)。上传文件会自动创建房间。在接收页输入配对码，或使用生成的接收链接。

## Windows 双击启动

Windows 需要安装 Docker Desktop。使用专用代理模式时，先运行配置所指向的代理程序，再双击项目目录中的 `启动CrossDrop.cmd`。启动器会检查 Docker，引擎未运行时尝试启动 Docker Desktop。随后启动服务，验证公网健康接口，取得当前可用网址并打开默认浏览器。首次连接可能需要等待几分钟。

可以在 Windows 桌面创建指向该 CMD 文件的快捷方式。启动器根据自身所在目录定位项目，整个项目目录需要一起保留。

启动器默认使用直连模式。专用代理模式可在项目 `.env` 中设置以下内容，代理地址需要填写为自己已检查可用的 SOCKS5 入口。

```dotenv
CROSSDROP_TUNNEL_MODE=proxy
CROSSDROP_PROXY_URL=socks5://host.docker.internal:实际端口
```

`direct` 使用基础 Compose 配置，`proxy` 追加 `compose.proxy.yaml`。该选择控制 CrossDrop 连接器的出网路径。启动器支持 Windows 与 Unix 换行的 `.env` 文件，遇到无效模式值会显示配置错误。

成功后，网址保存在项目的 `PUBLIC_URL.txt`。错误信息保存在 `STARTUP_LOG.txt`。启动器采用英文提示以提高 CMD 编码兼容性。公网检查失败时会继续等待，日志确认旧隧道失效后会尝试重启一次连接器。临时隧道重新创建时会生成新网址。

## 跨网络传输

需要临时在外网测试时，使用 Cloudflare Quick Tunnel。

```sh
docker compose --profile public up -d --build
docker compose logs --tail 80 cloudflared
```

从日志取得临时 HTTPS 地址。发送端和接收端都通过这个地址打开页面，再生成或使用配对码、链接或二维码。主机须保持运行。隧道重启可能生成新地址，浏览器按网址保存发送凭证。长期部署应使用自己的固定 HTTPS 入口。可选的 `compose.proxy.yaml` 仅给连接器配置代理，使用时先在 `.env` 中填写自己的 `CROSSDROP_PROXY_URL`。

## 数据与访问

服务默认只在主机的 `127.0.0.1:8787` 监听。文件经部署者主机中转并保存在 `./data`，部署者可以读取磁盘中的文件。配对码或接收链接可以访问房间文件，适合与信任的人分享。默认房间保留 24 小时，单文件上限 10 GiB，登记总量上限 50 GiB。合并时需为分块和最终文件同时预留磁盘空间。可复制 `.env.example` 为 `.env` 调整限制。

## 开发与验证

```sh
go test ./cmd/server
go vet ./cmd/server
npm --prefix web ci
npm --prefix web run build
node scripts/smoke.mjs
```

Docker 构建会运行 Go 测试和前端构建。Smoke test 需要先启动本地服务，通过 `BASE_URL` 可指定其他测试入口。后端测试覆盖房间权限、分块校验、重启恢复和清理逻辑。

## 许可证

[MIT](LICENSE)
