# 最终 SHA 对账

| 项 | SHA |
|---|---|
| 基线（3.5.24） | `84b013d23f0c11eaf86de9d5e776789467cb4192` |
| 任务书 commit | `5020ec557300a0dbdc4e6a1a2834d2b938da0f90` |
| 任务书 blob | `02bff3b3f4851be612cf9f883bbf616da3a29b6a` |
| G0/G1 冻结 | `74ef211170e1e9e8dcaac235750b6d3de2feae47` |
| 最终代码 SHA（G4 门禁锚定） | `0caf9da177e0cf9988ebcd0f10f43c5fc224` |
| 最终 HEAD（含 G5-G8 证据） | `714613be7e460b369f91cf52af67b2e244915240` |

- `0caf9da` → HEAD 之间仅 docs/evidence 与 G6 benchmark 文件变更；benchmark 为纯测试代码，不影响产品代码语义。
- 生产镜像 commit：3.5.24 `84b013d…`（未变）。
