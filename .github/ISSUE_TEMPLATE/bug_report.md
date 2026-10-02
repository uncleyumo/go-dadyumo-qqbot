---
name: 报告一个 Bug
about: 机器人行为不符合预期，或某条消息处理出错
title: "[Bug] "
labels: bug
assignees: ''
---

## 现象

描述你观察到的行为。比如「它把我@ 它的消息回了，但没@ 我」「发了图之后卡了十几秒才说话」。

## 复现步骤

1.
2.
3.

## 日志

贴管理端「实时日志」里对应时间段的行（`journalctl -u dadyumo-qqbot -n 100 --no-pager` 也可以）。

```
（粘贴日志，注意先删掉里面的 group_openid / user_openid）
```

## 环境

| 项 | 值 |
| --- | --- |
| 版本（commit） | |
| 部署方式 | systemd / 手动 run |
| `qq.sandbox` | true / false |
| 群里是否开了全量消息 | 是 / 否 |
| 触发的消息类型 | 纯文本 / 图片 / 视频 / 语音 |

## 涉密提醒

**贴日志前请确认里面没有真实 openid 或群号。** 回调日志会记录 `group_openid` 和 `user_openid`，这些是可直接定位到个人的标识符。

如果你在 `config.json` 里改过接入点，也**不要**把该文件贴上来。