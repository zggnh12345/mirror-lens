# 3.5.18 设计冻结稿 §15.3 正式授权修订记录（REV-1）

## 授权

用户裁定原文（2026-11-21）：「裁定：A，授权对冻结文档 §15.3 进行一次范围严格受限的权威修订，在性能维度增加 PERFORMANCE_NOT_MET……此次修改只扩充状态表达能力，不修改性能验收标准和计算规则。文档必须记录这是对原 FROZEN 文档的正式授权修订，更新版本记录、文件哈希和证据清单。不能静默覆盖原始冻结版本。」

## 修订内容

- 文件：`docs/3.5.18-design-freeze-errata-draft.md` §15.3（约 811-832 行）。
- 性能维度枚举：`PASS / PERFORMANCE_EXTERNAL_BLOCKED / NOT_EXECUTED`
  → `PASS / PERFORMANCE_NOT_MET / PERFORMANCE_EXTERNAL_BLOCKED / NOT_EXECUTED`。
- 新增「性能状态四值定义」表，明确 `PERFORMANCE_NOT_MET` = 已执行但未达标，
  既非外部阻塞、亦非未执行。
- 综合判定规则补充：该状态须在最终报告如实附带，不得宣称性能 PASS。
- 文档内嵌 `【正式授权修订 REV-1】` 标注，声明不触碰阈值/统计规则。

## 未改变（冻结边界）

- §12.3.0 P95 相对改善 ≥50% 阈值：未改。
- §12.5 流量一致性要求：未改。
- 10 轮、P50/P95/P99、线性插值、异常值处理：未改。
- 不允许挑选样本覆盖、不调整统计口径：遵守。

## 版本与哈希链

| 项 | 原 FROZEN | REV-1 修订后 |
|---|---|---|
| SHA-256 | `eded95cbae25d4fc4322e746224b73a40a223813d58a55321013ad206ad84eb3` | `51b6e0114722679aa821ecd9f45cae989ad84085d4d2f6ad945a22f4cf367ff0` |
| Git Blob | `f68a0bd8123635b902adf3072ad3609ff81c45e3` | `2ebdf9ff94fca0da93132722db8e536e3bd3308a` |
| 字节/行 | 86104 / 976 | 87802 / 989 |

- 原冻结文档全文备份：`docs/evidence/3.5.18/3.5.18-design-freeze-errata-draft.FROZEN-ORIGINAL.md`
- 原校验清单备份：`docs/evidence/3.5.18/3.5.18-design-freeze.FROZEN-ORIGINAL.sha256`
- 校验清单 `docs/3.5.18-design-freeze.sha256` 同时保留原值与 REV-1 值，并把两份备份登记在案。
- 复核命令：`sha256sum docs/3.5.18-design-freeze-errata-draft.md` /
  `git hash-object docs/3.5.18-design-freeze-errata-draft.md`。
