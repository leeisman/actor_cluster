# Project Vision (專案願景)

## 🎯 背景與「為什麼」要手刻框架

在開發現代高併發、高吞吐量的分散式系統（例如：高頻投注錢包）時，我們常常面臨極端的效能與一致性挑戰。雖然市面上有諸多成熟的 Actor 框架（如 Akka, Proto.Actor），但在追求**極致效能、拒絕重型黑盒子、以及對底層 GC 與記憶體佈局擁有完全控制權**的情境下，我們選擇手刻一套「純 Go、無外部 Actor 框架、專為高併發設計」的 Actor Cluster 系統。（註：我們僅引入如 gRPC、etcd client、gocql 等必備的基礎設施驅動，徹底屏棄肥大的封裝）。

本專案旨在解決以下核心痛點：
- 避免通用框架過度封裝帶來的隱藏效能損耗。
- 精準控制 Go Scheduler 的行為與 GC 壓力。
- 實現高度定製化的叢集分片 (Cluster Sharding) 與事件溯源 (Event Sourcing) 機制，確保系統達到 99.99% 高可用性。

## 🚀 技術願景

我們將構建一個基於 **Actor 模型與 Event Sourcing架構** 的底層框架，具備以下特性：
1. **極致效能與無鎖設計**：核心調度機制將揚棄傳統 Mutex 與 Go 原生 Channel（於熱點路徑），改採 MPSC (Multi-Producer Single-Consumer) Queue 與無鎖操作，榨乾硬體效能。
2. **高可用與容錯力**：結合 Cassandra 作為不可變的 Event Store，並利用 Cassandra 內的 Snapshot 行資料實現快速的狀態重建 (Rehydration/Replay)。
3. **無縫水平擴展**：具備位置透明性 (Location Transparency) 的 Cluster Sharding，無縫整合至 Kubernetes 中，隨流量波動靈活增減節點。

## 📈 成功指標 (Success Metrics)

- **吞吐量 (Throughput)**：單一 Actor 節點支援百萬級別別的消息處理 (Message Processing) 能力。
- **低延遲 (Latency)**：核心 Actor 通訊與狀態變更延遲控制在微秒級別。
- **高可用性 (Availability)**：在節點失效或 K8s pod 重新調度時，Actor 重建與狀態轉移時間極短，滿足 99.99% 的系統可用性。
- **資源使用率 (Resource Utilization)**：最低限度觸發 Go GC Mark-Sweep，透過記憶體池 (sync.Pool) 及嚴格的逃逸分析控制。
