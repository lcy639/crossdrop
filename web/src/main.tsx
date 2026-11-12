import React, { useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import QRCode from "qrcode";
import { sha256 } from "hash-wasm";
import "./style.css";

type Config = {
  chunkSize: number;
  maxFileBytes: number;
  maxFiles: number;
  roomTTLHours: number;
};
type Credentials = {
  id: string;
  code?: string;
  ownerToken?: string;
  receiveToken: string;
  expires: string;
};
type RemoteFile = {
  id: string;
  name: string;
  size: number;
  sha256: string;
  ready: boolean;
  chunks: number[];
  verifying: boolean;
  error?: string;
};
type Room = { id: string; expires: string; files: RemoteFile[] };
type Job = {
  key: string;
  file: File;
  progress: number;
  message: string;
  error?: string;
  done: boolean;
};
class ApiError extends Error {
  constructor(
    message: string,
    public status: number,
  ) {
    super(message);
  }
}
const storageKey = "crossdrop.sender.v1";
const defaultConfig: Config = {
  chunkSize: 8 * 1024 * 1024,
  maxFileBytes: 10 * 1024 ** 3,
  maxFiles: 100,
  roomTTLHours: 24,
};
const size = (value: number) => {
  if (value === 0) return "0 B";
  const unit = Math.min(3, Math.floor(Math.log(value) / Math.log(1024)));
  return `${(value / 1024 ** unit).toFixed(unit ? 1 : 0)} ${["B", "KB", "MB", "GB"][unit]}`;
};
const date = (value: string) =>
  new Date(value).toLocaleString("zh-CN", {
    month: "numeric",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
function savedSender(): Credentials | null {
  try {
    const value = JSON.parse(localStorage.getItem(storageKey) || "null");
    return value?.ownerToken && new Date(value.expires).getTime() > Date.now()
      ? value
      : null;
  } catch {
    return null;
  }
}
async function api<T>(
  path: string,
  token?: string,
  method = "GET",
  body?: unknown,
  signal?: AbortSignal,
): Promise<T> {
  const response = await fetch(path, {
    method,
    signal,
    headers: {
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(body ? { "Content-Type": "application/json" } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!response.ok) {
    const data = await response.json().catch(() => ({}));
    throw new ApiError(
      data.error || `请求失败 (${response.status})`,
      response.status,
    );
  }
  return response.status === 204 ? (undefined as T) : response.json();
}
function hashFile(
  file: File,
  signal: AbortSignal,
  progress: (n: number) => void,
): Promise<string> {
  return new Promise((resolve, reject) => {
    const worker = new Worker(new URL("./hash.worker.ts", import.meta.url), {
      type: "module",
    });
    const close = () => {
      worker.terminate();
      signal.removeEventListener("abort", abort);
    };
    const abort = () => {
      close();
      reject(new DOMException("已暂停", "AbortError"));
    };
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) {
      abort();
      return;
    }
    worker.onmessage = (event) => {
      if (event.data.error) {
        close();
        reject(new Error(event.data.error));
      } else if (event.data.hash) {
        close();
        resolve(event.data.hash);
      } else progress(event.data.progress);
    };
    worker.onerror = () => {
      close();
      reject(new Error("文件校验计算失败"));
    };
    worker.postMessage(file);
  });
}
function upload(
  url: string,
  token: string,
  chunk: Blob,
  hash: string,
  signal: AbortSignal,
  progress: (n: number) => void,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    const abort = () => xhr.abort();
    const cleanup = () => signal.removeEventListener("abort", abort);
    xhr.open("PUT", url);
    xhr.timeout = 180000;
    xhr.setRequestHeader("Authorization", `Bearer ${token}`);
    xhr.setRequestHeader("X-Chunk-SHA256", hash);
    xhr.upload.onprogress = (event) => progress(event.loaded);
    xhr.onload = () => {
      cleanup();
      if (xhr.status >= 200 && xhr.status < 300) resolve();
      else {
        let message = `上传失败 (${xhr.status})`;
        try {
          message = JSON.parse(xhr.responseText).error || message;
        } catch {
          /* A proxy may return HTML. */
        }
        reject(new ApiError(message, xhr.status));
      }
    };
    xhr.onerror = () => {
      cleanup();
      reject(new ApiError("网络中断，请检查连接", 0));
    };
    xhr.ontimeout = () => {
      cleanup();
      reject(new ApiError("上传超时", 0));
    };
    xhr.onabort = () => {
      cleanup();
      reject(new DOMException("已暂停", "AbortError"));
    };
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) {
      cleanup();
      reject(new DOMException("已暂停", "AbortError"));
      return;
    }
    xhr.send(chunk);
  });
}
function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const finish = () => {
      signal.removeEventListener("abort", abort);
      resolve();
    };
    const timer = setTimeout(finish, ms);
    const abort = () => {
      clearTimeout(timer);
      reject(new DOMException("已暂停", "AbortError"));
    };
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) abort();
  });
}
function App() {
  const [tab, setTab] = useState<"send" | "receive">("send");
  const [config, setConfig] = useState(defaultConfig);
  const [online, setOnline] = useState(false);
  const [sender, setSender] = useState<Credentials | null>(savedSender);
  const [receiver, setReceiver] = useState<Credentials | null>(null);
  const [sendRoom, setSendRoom] = useState<Room | null>(null);
  const [receiveRoom, setReceiveRoom] = useState<Room | null>(null);
  const [code, setCode] = useState("");
  const [jobs, setJobs] = useState<Job[]>([]);
  const [busy, setBusy] = useState(false);
  const [working, setWorking] = useState(false);
  const [notice, setNotice] = useState("");
  const [error, setError] = useState("");
  const [qr, setQr] = useState("");
  const [drag, setDrag] = useState(false);
  const input = useRef<HTMLInputElement>(null);
  const controller = useRef<AbortController | null>(null);
  const uploadBusy = useRef(false);
  const link = sender
    ? `${location.origin}/#receive=${sender.id}.${sender.receiveToken}`
    : "";

  useEffect(() => {
    api<Config>("/api/config")
      .then((value) => {
        setConfig(value);
        setOnline(true);
      })
      .catch(() => setError("服务连接失败，请刷新重试"));
    const params = new URLSearchParams(location.hash.slice(1));
    const share = params.get("receive");
    if (share) {
      const [id, receiveToken] = share.split(".");
      if (/^[a-f0-9]{32}$/.test(id) && /^[a-f0-9]{64}$/.test(receiveToken)) {
        setReceiver({ id, receiveToken, expires: "" });
        setTab("receive");
      } else setError("接收链接无效");
      history.replaceState(null, "", location.pathname);
    }
    return () => controller.current?.abort();
  }, []);
  useEffect(() => {
    try {
      sender
        ? localStorage.setItem(storageKey, JSON.stringify(sender))
        : localStorage.removeItem(storageKey);
    } catch {
      setNotice("浏览器未允许保存凭证，请保持此页面打开以继续上传");
    }
    if (link)
      QRCode.toDataURL(link, {
        width: 192,
        margin: 1,
        color: { dark: "#112b33", light: "#ffffff" },
      })
        .then(setQr)
        .catch(() => setQr(""));
    else setQr("");
  }, [sender, link]);
  useEffect(() => {
    if (!sender && !receiver) return;
    let active = true;
    const poll = async () => {
      const refresh = async (
        credentials: Credentials | null,
        update: (r: Room | null) => void,
      ) => {
        if (!credentials) return;
        try {
          const state = await api<Room>(
            `/api/rooms/${credentials.id}`,
            credentials.ownerToken || credentials.receiveToken,
          );
          if (active) {
            update(state);
            setOnline(true);
          }
        } catch (err) {
          if (!active) return;
          if (err instanceof ApiError && [401, 404, 410].includes(err.status)) {
            update(null);
            setError(err.message);
            if (credentials.ownerToken) setSender(null);
            else setReceiver(null);
          } else setOnline(false);
        }
      };
      await Promise.all([
        refresh(sender, setSendRoom),
        refresh(receiver, setReceiveRoom),
      ]);
    };
    void poll();
    const timer = setInterval(poll, 4000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, [sender, receiver]);

  const updateJob = (key: string, values: Partial<Job>) =>
    setJobs((current) =>
      current.map((job) => (job.key === key ? { ...job, ...values } : job)),
    );
  async function closeRoom() {
    if (!sender) return;
    setWorking(true);
    setError("");
    try {
      await api(`/api/rooms/${sender.id}`, sender.ownerToken, "DELETE");
      setSender(null);
      setSendRoom(null);
      setJobs([]);
      setNotice("房间已关闭，访问凭证已失效，文件将自动清理");
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setWorking(false);
    }
  }
  async function pair(event: React.FormEvent) {
    event.preventDefault();
    setWorking(true);
    setError("");
    setNotice("");
    try {
      setReceiver(
        await api<Credentials>("/api/pair", undefined, "POST", { code }),
      );
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setWorking(false);
    }
  }
  async function sendFiles(selected: File[]) {
    if (uploadBusy.current || working || !selected.length) return;
    if (selected.length > config.maxFiles) {
      setError(`一次最多选择 ${config.maxFiles} 个文件`);
      return;
    }
    const tooLarge = selected.find((file) => file.size > config.maxFileBytes);
    if (tooLarge) {
      setError(`${tooLarge.name} 超过单文件限制 ${size(config.maxFileBytes)}`);
      return;
    }
    uploadBusy.current = true;
    setBusy(true);
    setError("");
    setNotice("");
    const aborter = new AbortController();
    controller.current = aborter;
    const pending = selected.map((file, index) => ({
      key: `${Date.now()}-${index}`,
      file,
      progress: 0,
      message: "等待上传",
      done: false,
    }));
    setJobs(pending);
    try {
      let room = sender;
      if (!room?.ownerToken || new Date(room.expires).getTime() <= Date.now()) {
        room = await api<Credentials>("/api/rooms", undefined, "POST");
        setSender(room);
        setSendRoom({ id: room.id, expires: room.expires, files: [] });
      }
      const token = room.ownerToken!;
      for (const job of pending) {
        if (aborter.signal.aborted) break;
        try {
          updateJob(job.key, { message: "计算 SHA-256…" });
          const hash = await hashFile(job.file, aborter.signal, (progress) =>
            updateJob(job.key, {
              message: `计算 SHA-256 · ${Math.round(progress * 100)}%`,
            }),
          );
          const base = `/api/rooms/${room.id}/files`;
          const remote = await api<RemoteFile>(
            base,
            token,
            "POST",
            { name: job.file.name, size: job.file.size, sha256: hash },
            aborter.signal,
          );
          if (!remote.ready) {
            const uploaded = new Set(remote.error ? [] : remote.chunks);
            let completed = Array.from(uploaded).reduce(
              (n, index) =>
                n +
                Math.min(
                  config.chunkSize,
                  job.file.size - index * config.chunkSize,
                ),
              0,
            );
            const count = Math.ceil(job.file.size / config.chunkSize);
            for (let i = 0; i < count; i++) {
              if (uploaded.has(i)) continue;
              const chunk = job.file.slice(
                i * config.chunkSize,
                (i + 1) * config.chunkSize,
              );
              const digest = await sha256(
                new Uint8Array(await chunk.arrayBuffer()),
              );
              for (let attempt = 0; ; attempt++) {
                updateJob(job.key, {
                  progress: completed / Math.max(1, job.file.size),
                  message: `上传分块 ${i + 1} / ${count}${attempt ? ` · 第 ${attempt} 次重试` : ""}`,
                });
                try {
                  await upload(
                    `${base}/${remote.id}/chunks/${i}`,
                    token,
                    chunk,
                    digest,
                    aborter.signal,
                    (bytes) =>
                      updateJob(job.key, {
                        progress:
                          (completed + bytes) / Math.max(1, job.file.size),
                      }),
                  );
                  break;
                } catch (err) {
                  if (
                    aborter.signal.aborted ||
                    (err instanceof ApiError &&
                      err.status > 0 &&
                      err.status < 500 &&
                      err.status !== 429)
                  )
                    throw err;
                  updateJob(job.key, {
                    message: "连接中断，正在确认服务器接收状态…",
                  });
                  try {
                    const state = await api<Room>(
                      `/api/rooms/${room.id}`,
                      token,
                      "GET",
                      undefined,
                      aborter.signal,
                    );
                    const file = state.files.find(
                      (file) => file.id === remote.id,
                    );
                    if (file?.ready || file?.chunks.includes(i)) break;
                  } catch {
                    // A failed status check leaves the chunk eligible for retry.
                  }
                  if (aborter.signal.aborted || attempt >= 3) throw err;
                  updateJob(job.key, { message: "正在等待重试…" });
                  await sleep(1000 * 2 ** attempt, aborter.signal);
                }
              }
              completed += chunk.size;
            }
            updateJob(job.key, {
              progress: 1,
              message: "服务器正在合并并校验 SHA-256…",
            });
            await api(
              `${base}/${remote.id}/complete`,
              token,
              "POST",
              undefined,
              aborter.signal,
            );
            for (;;) {
              const state = await api<Room>(
                `/api/rooms/${room.id}`,
                token,
                "GET",
                undefined,
                aborter.signal,
              );
              const file = state.files.find((file) => file.id === remote.id);
              if (file?.ready) break;
              if (file?.error) throw new Error(file.error);
              if (!file?.verifying)
                throw new Error("校验已中断，请点击继续上传");
              await sleep(1500, aborter.signal);
            }
          }
          updateJob(job.key, {
            progress: 1,
            message: "传输完成 · SHA-256 已校验",
            done: true,
          });
        } catch (err) {
          if (aborter.signal.aborted) {
            updateJob(job.key, { message: "已暂停，点击继续上传" });
            break;
          }
          updateJob(job.key, {
            error: (err as Error).message,
            message: "上传未完成，已保留成功分块",
          });
        }
      }
      setSendRoom(await api<Room>(`/api/rooms/${room.id}`, token));
    } catch (err) {
      setError((err as Error).message);
    } finally {
      uploadBusy.current = false;
      setBusy(false);
      controller.current = null;
    }
  }
  async function copy(text: string) {
    try {
      await navigator.clipboard.writeText(text);
      setNotice("已复制到剪贴板");
    } catch {
      setNotice("复制不可用，请长按选择下方链接或配对码");
    }
  }
  const files =
    tab === "send" ? sendRoom?.files || [] : receiveRoom?.files || [];
  const activeRoom = tab === "send" ? sendRoom : receiveRoom;
  return (
    <div className="app">
      <header>
        <a className="brand" href="/" aria-label="CrossDrop 首页">
          <span className="brand-icon">↗</span>CrossDrop
          <span className="brand-dot">.</span>
        </a>
        <span className={`connection ${online ? "online" : ""}`}>
          <i />
          {online ? "服务已连接" : "连接检查中"}
        </span>
      </header>
      <main>
        <section className="intro">
          <p className="eyebrow">YOUR DEVICES. ONE CONNECTION.</p>
          <h1>
            文件，<span>自由抵达。</span>
          </h1>
          <p className="subtitle">
            跨越设备与网络，把文件送到对的人手中。
            <br className="mobile-break" />
            打开浏览器，即刻连接。
          </p>
        </section>
        <nav className="tabs" aria-label="传输模式">
          <button
            className={tab === "send" ? "selected" : ""}
            onClick={() => {
              setTab("send");
              setError("");
            }}
          >
            ↗ 发送文件
          </button>
          <button
            className={tab === "receive" ? "selected" : ""}
            onClick={() => {
              setTab("receive");
              setError("");
            }}
          >
            ↙ 接收文件
          </button>
        </nav>
        {error && (
          <div className="alert error" role="alert">
            {error}
            <button onClick={() => setError("")} aria-label="关闭错误提示">
              ×
            </button>
          </div>
        )}
        {notice && (
          <div className="alert" role="status">
            {notice}
            <button onClick={() => setNotice("")} aria-label="关闭提示">
              ×
            </button>
          </div>
        )}
        <div
          className={`workspace ${tab === "receive" && receiveRoom ? "connected" : ""}`}
        >
          <section className="panel transfer-panel">
            <div className="panel-heading">
              <div>
                <span className="step">
                  01 / {tab === "send" ? "SEND" : "RECEIVE"}
                </span>
                <h2>
                  {tab === "send" ? "选择要发送的文件" : "连接你的传输房间"}
                </h2>
              </div>
              <span className="pill">端到服务器</span>
            </div>
            {tab === "send" ? (
              <>
                <input
                  ref={input}
                  aria-label="选择上传文件"
                  type="file"
                  multiple
                  hidden
                  onChange={(event) => {
                    void sendFiles(Array.from(event.target.files || []));
                    event.target.value = "";
                  }}
                />
                <div
                  className={`dropzone ${drag ? "dragging" : ""}`}
                  onDragOver={(event) => {
                    event.preventDefault();
                    setDrag(true);
                  }}
                  onDragLeave={() => setDrag(false)}
                  onDrop={(event) => {
                    event.preventDefault();
                    setDrag(false);
                    void sendFiles(Array.from(event.dataTransfer.files));
                  }}
                >
                  <div className="large-icon">↥</div>
                  <h3>把文件拖到这里</h3>
                  <p>或从设备中选择文件，支持一次选择多个</p>
                  <button
                    className="primary"
                    onClick={() => input.current?.click()}
                    disabled={busy || working}
                  >
                    选择文件 <span>＋</span>
                  </button>
                  <small>
                    单文件最高 {size(config.maxFileBytes)} · 8 MB 分块续传
                  </small>
                </div>
                <div className="queue-heading">
                  <h3>
                    传输队列 <span>{jobs.length}</span>
                  </h3>
                  {busy ? (
                    <button
                      className="text-button"
                      onClick={() => controller.current?.abort()}
                    >
                      暂停上传
                    </button>
                  ) : jobs.some((job) => !job.done) ? (
                    <button
                      className="text-button"
                      onClick={() =>
                        void sendFiles(
                          jobs
                            .filter((job) => !job.done)
                            .map((job) => job.file),
                        )
                      }
                    >
                      继续上传 ↗
                    </button>
                  ) : null}
                </div>
                {jobs.map((job) => (
                  <div className="job" key={job.key}>
                    <div className="file-symbol">{job.done ? "✓" : "↑"}</div>
                    <div className="file-details">
                      <div className="file-title">
                        <strong>{job.file.name}</strong>
                        <span>{Math.round(job.progress * 100)}%</span>
                      </div>
                      <p className={job.error ? "job-error" : ""}>
                        {job.error || job.message} · {size(job.file.size)}
                      </p>
                      <progress
                        value={job.progress}
                        max="1"
                        aria-label={`${job.file.name} 上传进度`}
                      />
                    </div>
                  </div>
                ))}
                {!jobs.length && (
                  <p className="quiet-hint">
                    刷新页面后，重新选择相同文件即可恢复已上传的分块。
                  </p>
                )}
              </>
            ) : (
              <>
                {!receiveRoom && (
                  <form className="pair-form" onSubmit={pair}>
                    <div className="large-icon">↙</div>
                    <label htmlFor="pair-code">输入发送端的六位配对码</label>
                    <input
                      id="pair-code"
                      inputMode="numeric"
                      pattern="[0-9]{6}"
                      autoComplete="off"
                      placeholder="000000"
                      maxLength={6}
                      value={code}
                      onChange={(event) =>
                        setCode(event.target.value.replace(/\D/g, ""))
                      }
                    />
                    <button
                      className="primary"
                      disabled={working || code.length !== 6}
                    >
                      {working ? "正在连接…" : "连接房间 →"}
                    </button>
                    <p>也可以扫描发送端二维码，或打开接收链接。</p>
                  </form>
                )}
                {receiveRoom && (
                  <div className="connected-banner">
                    <i />
                    已连接房间 · 文件列表每 4 秒自动更新
                    <button
                      className="text-button"
                      onClick={() => {
                        setReceiver(null);
                        setReceiveRoom(null);
                        setCode("");
                      }}
                    >
                      连接其他房间
                    </button>
                  </div>
                )}
              </>
            )}
            <div className="panel-bottom">
              <span>◈ SHA-256 完整性校验</span>
              <span>↻ 支持断点续传</span>
            </div>
          </section>
          <aside className="panel share-panel">
            <span className="step">
              02 / {tab === "send" ? "CONNECT" : "DOWNLOAD"}
            </span>
            <h2>{tab === "send" ? "把连接，分享出去" : "等待文件抵达"}</h2>
            {tab === "send" ? (
              sender ? (
                <>
                  <p className="muted">
                    在另一台设备输入配对码，即可接收文件。
                  </p>
                  <button
                    className="pair-code"
                    title="复制配对码"
                    onClick={() => void copy(sender.code || "")}
                  >
                    {sender.code}
                  </button>
                  <div className="code-caption">六位配对码 · 点击复制</div>
                  <div className="qr-row">
                    {qr && (
                      <img
                        src={qr}
                        alt="扫码打开接收链接"
                        width="120"
                        height="120"
                      />
                    )}
                    <div>
                      <strong>扫一扫，直接接收</strong>
                      <p>
                        iPad / Android / 电脑
                        <br />
                        使用任意现代浏览器
                      </p>
                    </div>
                  </div>
                  <label className="link-label" htmlFor="share-link">
                    专属接收链接
                  </label>
                  <div className="link-box">
                    <input
                      id="share-link"
                      readOnly
                      value={link}
                      onFocus={(event) => event.target.select()}
                    />
                    <button onClick={() => void copy(link)}>复制</button>
                  </div>
                  <p className="expiry">房间有效至 {date(sender.expires)}</p>
                  <button
                    className="text-button danger"
                    disabled={busy || working}
                    onClick={closeRoom}
                  >
                    关闭房间并清理文件
                  </button>
                </>
              ) : (
                <div className="share-empty">
                  <div className="placeholder-code">··· ···</div>
                  <p>
                    选择文件后，配对码和二维码
                    <br />
                    会出现在这里。
                  </p>
                  <span className="small-note">每个房间拥有独立访问凭证</span>
                </div>
              )
            ) : (
              <div className="receive-summary">
                <div className="large-icon">⇣</div>
                <strong>
                  {receiveRoom
                    ? `${files.filter((file) => file.ready).length} 个文件已就绪`
                    : "还没有连接房间"}
                </strong>
                <p>
                  {receiveRoom
                    ? "校验通过的文件会显示下载按钮。"
                    : "输入配对码后，文件会显示在下方。"}
                </p>
              </div>
            )}
            <div className="privacy-note">
              <span>◉</span>
              <p>
                文件保存在部署者的本地磁盘。
                <br />
                配对码与接收链接可访问房间内全部文件，请仅分享给接收人。
              </p>
            </div>
          </aside>
        </div>
        {files.length > 0 && (
          <section className="panel files-panel">
            <div className="panel-heading">
              <div>
                <span className="step">TRANSFER FILES</span>
                <h2>
                  房间中的文件{" "}
                  <span className="file-count">{files.length}</span>
                </h2>
              </div>
              {activeRoom && (
                <span className="muted">有效至 {date(activeRoom.expires)}</span>
              )}
            </div>
            <div className="file-list">
              {files.map((file) => (
                <article className="stored-file" key={file.id}>
                  <div className="file-symbol">{file.ready ? "✓" : "↑"}</div>
                  <div className="file-details">
                    <strong>{file.name}</strong>
                    <p>
                      {size(file.size)} ·{" "}
                      {file.ready
                        ? "SHA-256 校验通过"
                        : file.error ||
                          (file.verifying
                            ? "服务器正在合并并校验…"
                            : `已收到 ${file.chunks.length} / ${Math.ceil(file.size / config.chunkSize)} 个分块`)}
                    </p>
                    <details>
                      <summary>SHA-256</summary>
                      <code>{file.sha256}</code>
                    </details>
                  </div>
                  {file.ready ? (
                    <a
                      className="download"
                      href={`/api/rooms/${activeRoom!.id}/files/${file.id}/download?token=${(tab === "send" ? sender : receiver)!.receiveToken}`}
                      download={file.name}
                      referrerPolicy="no-referrer"
                    >
                      下载 ↓
                    </a>
                  ) : (
                    <span className="pending">上传中</span>
                  )}
                </article>
              ))}
            </div>
          </section>
        )}
        <section className="features">
          <div>
            <b>01</b>
            <strong>无需同一网络</strong>
            <p>通过公网链接连接彼此</p>
          </div>
          <div>
            <b>02</b>
            <strong>为大文件而生</strong>
            <p>分块传输，断开后继续</p>
          </div>
          <div>
            <b>03</b>
            <strong>设备之间，自在切换</strong>
            <p>Windows · iPad · Android · Linux</p>
          </div>
        </section>
      </main>
      <footer>
        <span>CrossDrop · 你的私人文件中转站</span>
        <span>浏览器保持前台，可获得更稳定的移动端传输体验</span>
      </footer>
    </div>
  );
}

createRoot(document.getElementById("root")!).render(<App />);
