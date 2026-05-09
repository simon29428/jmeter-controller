# jmeter-controller

一個透過 `TestRun` 自訂資源管理分散式 JMeter 壓力測試的 Kubernetes Controller。Controller 會依設定的 run group 建立 worker（slave）Pod，等待所有 worker 進入 Ready 狀態後，選擇性地啟動一個 master Pod 來協調測試執行。

## 目錄

- [架構](#架構)
- [快速開始](#快速開始)
- [TestRun 資源](#testrun-資源)
  - [spec.slave](#specslave)
  - [spec.master](#specmaster)
  - [spec.runGroups](#specrungroups)
  - [掛載設定（ConfigMap / PVC）](#掛載設定configmap--pvc)
  - [Status 狀態](#status-狀態)
  - [Phase 生命週期](#phase-生命週期)
- [Controller 設定](#controller-設定)
  - [runGroupLimits](#rungrouplimits)
  - [podTemplate（slave）](#podtemplateslave)
  - [masterPodTemplate](#masterpodtemplate)
- [Pod 注入的環境變數](#pod-注入的環境變數)
- [REST API](#rest-api)
- [部署](#部署)

---

## 架構

```
TestRun CR
    │
    ├─ Worker Pod × N  （每個 runGroup 依 ceil(thread/base) 建立）
    │      └─ 等待所有 Worker Ready 且取得 IP
    │
    └─ Master Pod × 1  （所有 Worker Ready 後建立）
           └─ SLAVE_HOSTS = 逗號分隔的 Worker IP 列表
```

**每個 Pod 套用的 Label：**

| Label | 值 |
|---|---|
| `jmeter.jmeter.io/testrun` | TestRun 名稱 |
| `jmeter.jmeter.io/rungroup` | Run group 名稱（僅 worker）|
| `jmeter.jmeter.io/role` | `worker` 或 `master` |

---

## 快速開始

```bash
# 套用 CRD
kubectl apply -f config/crd/jmeter.jmeter.io_testruns.yaml

# 部署 Controller
kubectl apply -f config/manager/manager.yaml

# 執行測試（僅 worker，無 master）
kubectl apply -f config/samples/testrun_sample2.yaml

# 執行測試（master + worker）
kubectl apply -f config/samples/testrun_sample.yaml

# 查看狀態
kubectl get tr -A
kubectl describe tr example-testrun
```

---

## TestRun 資源

```yaml
apiVersion: jmeter.jmeter.io/v1
kind: TestRun
metadata:
  name: my-test
  namespace: default
spec:
  slave:            # 必填
    image: alpine/jmeter:5.6
    mounts: []      # 選填

  master:           # 選填 — 省略則不建立 master pod
    image: alpine/jmeter:5.6
    scriptPath: /scripts/test-plan.jmx
    mounts: []      # 選填

  runGroups:        # 必填，至少一個 run group
    group-a:
      thread: 120
      base: 50
      nodeSelector:
        kubernetes.io/os: linux
```

### spec.slave

| 欄位 | 型別 | 必填 | 說明 |
|---|---|---|---|
| `image` | string | ✅ | Worker pod 使用的容器映像 |
| `mounts` | []MountSpec | — | 掛載到 worker pod 的 Volume（見[掛載設定](#掛載設定configmap--pvc)）|

### spec.master

省略此區塊即為純 worker 模式，不建立 master pod。

| 欄位 | 型別 | 必填 | 說明 |
|---|---|---|---|
| `image` | string | ✅ | Master pod 使用的容器映像 |
| `scriptPath` | string | ✅ | Master 容器內 `.jmx` 測試腳本的路徑，以 `SCRIPT_PATH` 環境變數注入 |
| `mounts` | []MountSpec | — | 掛載到 master pod 的 Volume（見[掛載設定](#掛載設定configmap--pvc)）|

### spec.runGroups

一個具名 run group 的 map，每個 group 獨立建立一組 worker pod。

| 欄位 | 型別 | 必填 | 說明 |
|---|---|---|---|
| `thread` | int32 | ✅ | 此 group 的總虛擬執行緒數 |
| `base` | int32 | — | 每個 pod 分配的執行緒數（預設 `50`）。Pod 數量 = `ceil(thread / base)` |
| `nodeSelector` | map[string]string | — | 限制 pod 排程到指定節點的 label 條件 |

**Pod 數量計算範例：**

```
thread: 120, base: 50  →  3 個 pod：[50t, 50t, 20t]
thread: 80             →  2 個 pod：[50t, 30t]  （base 預設為 50）
```

### 掛載設定（ConfigMap / PVC）

`slave.mounts` 與 `master.mounts` 皆接受 `MountSpec` 列表：

| 欄位 | 型別 | 必填 | 說明 |
|---|---|---|---|
| `name` | string | ✅ | Kubernetes Volume 名稱（在同一 pod 內須唯一）|
| `mountPath` | string | ✅ | 容器內的掛載絕對路徑 |
| `configMap` | string | — | 要掛載的 ConfigMap 名稱，與 `pvc` 互斥 |
| `pvc` | string | — | 要掛載的 PersistentVolumeClaim 名稱，與 `configMap` 互斥 |

**範例：**

```yaml
slave:
  image: alpine/jmeter:5.6
  mounts:
    - name: jmeter-scripts
      mountPath: /scripts
      configMap: my-scripts-configmap   # 掛載 ConfigMap
    - name: test-data
      mountPath: /data
      pvc: test-data-pvc                # 掛載 PVC

master:
  image: alpine/jmeter:5.6
  scriptPath: /scripts/test-plan.jmx
  mounts:
    - name: jmeter-scripts
      mountPath: /scripts
      configMap: my-scripts-configmap
```

> Controller 層級的 `podTemplate` / `masterPodTemplate` 的掛載會優先套用，`spec.slave.mounts` 與 `spec.master.mounts` 為疊加補充，相同 volume 名稱不會重複加入。

### Status 狀態

```yaml
status:
  phase: WorkersReady          # 見 Phase 生命週期
  message: "All workers ready, waiting for master pod to start"
  startTime: "2026-05-09T10:00:00Z"
  pods:                        # worker pod 清單
    - name: my-test-group-a-0
      ip: 10.0.0.1
      runGroup: group-a
      threadCount: 50
      phase: Running
  masterPod:                   # master pod 建立後才存在
    name: my-test-master
    ip: 10.0.0.10
    phase: Running
```

### Phase 生命週期

#### 純 Worker 模式（省略 `spec.master`）

```
Pending → Running → Completed / Failed
```

#### Master + Worker 模式

```
Pending → WorkersReady → Running → Completed / Failed
              ↑
    （所有 worker Ready 且取得 IP）
```

| Phase | 說明 |
|---|---|
| `Pending` | Worker pod 建立中或尚未 Ready |
| `Waiting` | 達到併發執行限制，排隊等待中 |
| `WorkersReady` | 所有 worker 已 Ready；master pod 建立中或啟動中 |
| `Running` | Master pod 正在執行中 |
| `Completed` | Master pod 成功結束，且所有 worker pod 均已完成且無失敗 |
| `Failed` | Master pod 或至少一個 worker pod 失敗 |

---

## Controller 設定

透過 `--config=<路徑>` 載入 YAML 設定檔。所有區塊皆為選填。

```yaml
# config/samples/controller_config.yaml

runGroupLimits:
  group-a: 2   # run group "group-a" 最多同時執行 2 個 TestRun
  group-b: 1

podTemplate:          # slave pod 的基礎模板
  ...

masterPodTemplate:    # master pod 的基礎模板
  ...
```

### runGroupLimits

限制共用相同 run group 名稱的 TestRun 可同時處於活躍狀態（`Pending`、`WorkersReady`、`Running`）的數量。超過限制的 TestRun 會進入 `Waiting` 狀態，每 30 秒重試一次。

未列出的 run group 不受限制。

### podTemplate（slave）

套用到每個 **worker pod** 的基礎 `PodTemplateSpec`。Controller 會強制在模板之上覆蓋以下設定：

| 強制覆蓋欄位 | 值 |
|---|---|
| Labels | `jmeter.jmeter.io/testrun`、`jmeter.jmeter.io/rungroup`、`jmeter.jmeter.io/role=worker` |
| `spec.restartPolicy` | `Never` |
| 容器 `name` | `jmeter-slave`（不存在時自動建立）|
| 容器 `image` | TestRun 的 `spec.slave.image` |
| 環境變數 | `TESTRUN_NAME`、`RUN_GROUP`、`THREAD_COUNT`（見下方說明）|

**範例：**

```yaml
podTemplate:
  metadata:
    annotations:
      prometheus.io/scrape: "false"
  spec:
    terminationGracePeriodSeconds: 30
    tolerations:
      - key: "jmeter"
        operator: "Equal"
        value: "slave"
        effect: "NoSchedule"
    containers:
      - name: jmeter-slave       # 必須命名為 "jmeter-slave"
        resources:
          requests:
            cpu: "500m"
            memory: "512Mi"
          limits:
            cpu: "2"
            memory: "2Gi"
        volumeMounts:
          - name: shared-data
            mountPath: /data
    volumes:
      - name: shared-data
        emptyDir: {}
```

### masterPodTemplate

結構與 `podTemplate` 相同，但套用於 **master pod**。

| 強制覆蓋欄位 | 值 |
|---|---|
| Labels | `jmeter.jmeter.io/testrun`、`jmeter.jmeter.io/role=master` |
| `spec.restartPolicy` | `Never` |
| 容器 `name` | `jmeter-master`（不存在時自動建立）|
| 容器 `image` | TestRun 的 `spec.master.image` |
| 環境變數 | `TESTRUN_NAME`、`SLAVE_HOSTS`、`SCRIPT_PATH`（見下方說明）|

**範例：**

```yaml
masterPodTemplate:
  metadata:
    annotations:
      prometheus.io/scrape: "false"
  spec:
    terminationGracePeriodSeconds: 30
    tolerations:
      - key: "jmeter"
        operator: "Equal"
        value: "master"
        effect: "NoSchedule"
    containers:
      - name: jmeter-master      # 必須命名為 "jmeter-master"
        resources:
          requests:
            cpu: "500m"
            memory: "512Mi"
          limits:
            cpu: "2"
            memory: "2Gi"
```

---

## Pod 注入的環境變數

### Worker（slave）Pod

| 變數 | 值 |
|---|---|
| `TESTRUN_NAME` | TestRun 名稱 |
| `RUN_GROUP` | Run group 名稱 |
| `THREAD_COUNT` | 分配給此 pod 的執行緒數 |

### Master Pod

| 變數 | 值 |
|---|---|
| `TESTRUN_NAME` | TestRun 名稱 |
| `SLAVE_HOSTS` | 所有 worker pod IP 的逗號分隔列表（例如 `10.0.0.1,10.0.0.2`）|
| `SCRIPT_PATH` | `spec.master.scriptPath` 的值 |

---

## REST API

Controller 在 `--api-port`（預設 `8080`）提供輕量 HTTP API。

| 方法 | 路徑 | 說明 |
|---|---|---|
| `GET` | `/api/v1/testruns` | 列出所有命名空間的 TestRun |
| `GET` | `/api/v1/namespaces/{namespace}/testruns` | 列出指定命名空間的 TestRun |
| `GET` | `/api/v1/namespaces/{namespace}/testruns/{name}/pods` | 列出指定 TestRun 的所有 Pod |
| `POST` | `/api/v1/namespaces/{namespace}/testruns/{name}/stop` | 刪除 TestRun（停止測試）|

---

## 部署

Controller 從透過 ConfigMap 掛載的設定檔讀取設定，使用 `--config` 指定檔案路徑。

**啟動參數：**

| 參數 | 預設值 | 說明 |
|---|---|---|
| `--config` | `""` | Controller 設定檔的 YAML 路徑 |
| `--api-port` | `8080` | REST API 伺服器的監聽埠 |
| `--metrics-bind-address` | `:8081` | Prometheus metrics 端點 |
| `--health-probe-bind-address` | `:8082` | Liveness / Readiness 探針端點 |
| `--leader-elect` | `false` | 啟用 leader election 以支援高可用部署 |

完整部署範例請參考 [config/manager/manager.yaml](config/manager/manager.yaml)。
