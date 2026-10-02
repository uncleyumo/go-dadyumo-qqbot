#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
从 systemd 日志里导出「羽沫老爹」的群聊对话记录，按群分节输出 Markdown，供行为分析用。

用法：
    ssh <服务器别名> "journalctl -u dadyumo-qqbot --no-pager -o cat" > /tmp/journal.raw.log
    scp <服务器别名>:/opt/dadyumo-qqbot/data/memory.json /tmp/memory.json
    python3 scripts/export_chatlog.py --journal /tmp/journal.raw.log \
        --memory /tmp/memory.json --out docs/chat-logs/2026-09-30-群聊对话记录.md

两个已知的数据源局限，分析时必须记住：
  1. 收到的消息在日志里被 truncate(content, 60) 截断（超过 60 字以 … 结尾），
     所以长消息只能看到开头。改大要动 internal/agent/agent.go 的日志行。
  2. 日志只记「发生了什么」，不记「为什么不说」。没发话的轮次没有记录，
     沉默本身无法从日志还原，只能靠统计推断。

安全：只抽取白名单内的日志类型，**绝不会**把 [Debug] 行（含 appSecret / access_token）写进文档。
"""

import argparse
import json
import os
import re
import sys
from collections import Counter, OrderedDict, defaultdict

# 要写进文档的日志类型
INCOMING = "群消息"
INCOMING_C2C = "单聊消息"
SPOKE = "已发言"
SEND_FAIL = "发言发送失败"
PROACTIVE_TRY = "群里冷场，尝试主动找话"
DEDUP = "检测到重复发言"
SUMMARY = "已更新前文提要"

BOOT = "羽沫老爹启动中"
CFG_SAVED = "配置已写入"
LOGIN = "管理端登录成功"
MASTER_BOUND = "已绑定主人"
MASTER_BOUND_ADMIN = "管理端绑定主人"
ENV_REFRESH = "环境信息已刷新"
SCHEDULE = "在线时段调度已启用"
CALL_FAIL = "调用失败，切换到下一个目标"
CALL_RETRY = "调用经重试后成功"
PARSE_FAIL = "群机器人事件解析失败"
SIGN_FAIL = "回调签名校验失败，已拒绝"

SELFTEST = "SELFTEST_GROUP_NOT_REAL"

ROLE_BOT = "羽沫老爹"


def load(path):
    recs = []
    if not path or not os.path.exists(path):
        return recs
    with open(path, encoding="utf-8", errors="replace") as f:
        for line in f:
            line = line.strip()
            if not line.startswith("{"):
                continue  # systemd 自己的行、[Debug] 行，一律跳过
            try:
                d = json.loads(line)
            except Exception:
                continue
            if isinstance(d, dict) and "ts" in d and "msg" in d:
                recs.append(d)
    return recs


def short_id(oid):
    return oid[-8:] if len(oid) > 8 else oid


def is_selftest(g):
    """管理端自检用的是假 openid（尾部恰好是 NOT_REAL），不是真实群，一律排除"""
    return not g or "SELFTEST" in g or g == "NOT_REAL"


def redact(s):
    """认主口令这类东西不该进文档（日志里是明文）"""
    s = re.sub(r"(#认主\s+)\S+", r"\1<口令已脱敏>", str(s))
    return s


def err_code(s):
    m = re.search(r'"code":(\d+)', s or "")
    if m:
        return m.group(1)
    m = re.search(r"code:(\d+)", s or "")
    return m.group(1) if m else ""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--journal", required=True)
    ap.add_argument("--memory", default="")
    ap.add_argument("--out", required=True)
    ap.add_argument("--persona", default="羽沫老爹")
    ap.add_argument("--alias", default="", help="群别名，格式：00000000=示例群,11111111=测试群")
    args = ap.parse_args()

    recs = load(args.journal)
    if not recs:
        sys.exit("日志里没有解析出任何记录")

    recs.sort(key=lambda d: d["ts"])
    t0, t1 = recs[0]["ts"], recs[-1]["ts"]

    # ---- 长期记忆：群名、成员、要点 ----
    mem_groups = {}
    if args.memory and os.path.exists(args.memory):
        with open(args.memory, encoding="utf-8") as f:
            mem = json.load(f)
        for g in mem.get("groups", []):
            mem_groups[g.get("openid", "")] = g

    # ---- openid -> 昵称（私聊日志只有 openid，得靠长期记忆翻译）----
    name_by_oid = {}
    for g in mem_groups.values():
        for m in (g.get("members") or {}).values():
            if m.get("openid"):
                name_by_oid[m["openid"]] = m.get("name") or short_id(m["openid"])

    def who_name(oid):
        return name_by_oid.get(oid, short_id(oid))

    # ---- openid 归组：日志里群消息只打短 ID，发言打全 ID，按后缀对齐 ----
    full_ids = OrderedDict()
    for d in recs:
        g = (d.get("kv") or {}).get("group")
        if not g or is_selftest(g) or g.startswith("c2c:"):
            continue
        if len(g) > 8:
            full_ids.setdefault(short_id(g), g)
    for k, v in full_ids.items():
        mem_groups.setdefault(v, {"openid": v, "name": k, "members": {}, "facts": {}})

    # ---- 群别名：配置里 groups 是空的，日志只有 openid 后 8 位，
    #      想让文档可读就用 --alias 00000000=示例群 补上 ----
    aliases = {}
    for item in (args.alias or "").split(","):
        if "=" in item:
            k, v = item.split("=", 1)
            aliases[k.strip()] = v.strip()

    def group_key(g):
        """把群消息里的短 ID 还原成完整 openid"""
        if not g:
            return ""
        if g.startswith("c2c:"):
            return g
        if len(g) > 8:
            return g
        return full_ids.get(g, g)

    def group_label(g):
        if g in aliases:
            return aliases[g]
        if short_id(g) in aliases:
            return aliases[short_id(g)]
        mg = mem_groups.get(g, {})
        name = mg.get("name") or short_id(g)
        return name if name != short_id(g) else short_id(g)

    # ---- 分群时间线 ----
    timeline = defaultdict(list)   # group -> [(ts, role, text, note)]
    c2c_timeline = []              # 私聊单独一节
    stats = defaultdict(lambda: Counter())
    system_events = []
    boots = []

    for d in recs:
        kv = d.get("kv") or {}
        msg, ts = d["msg"], d["ts"]

        if msg == INCOMING:
            g = group_key(kv.get("group"))
            if is_selftest(g):
                system_events.append((ts, "自检", "管理端自检事件（假 openid，非真实群），已从对话记录中排除"))
                continue
            stats[g]["in"] += 1
            if kv.get("at"):
                stats[g]["at"] += 1
            note = "被@" if kv.get("at") else ""
            text = kv.get("text") or ""
            if not text.strip():
                text, note = "（空消息，多半是图片/表情或纯@）", note
            timeline[g].append((ts, kv.get("from") or "?", text, note))

        elif msg == INCOMING_C2C:
            c2c_timeline.append((ts, who_name(kv.get("from") or ""), kv.get("text") or "", "私聊"))

        elif msg == SPOKE:
            g = kv.get("group") or ""
            text, segs = kv.get("内容") or "", kv.get("条数") or 1
            if g.startswith("c2c:"):
                c2c_timeline.append((ts, ROLE_BOT, text, f"{segs} 条"))
            else:
                g = group_key(g)
                stats[g]["spoke"] += 1
                stats[g]["segs"] += int(segs or 0)
                timeline[g].append((ts, ROLE_BOT, text, f"{segs} 条"))

        elif msg == SEND_FAIL:
            g = group_key(kv.get("group"))
            if is_selftest(g):
                continue
            stats[g]["fail"] += 1
            stats[g]["fail_" + (err_code(kv.get("err")) or "?")] += 1
            timeline[g].append((ts, ROLE_BOT, kv.get("seg") or "",
                                "发送失败 " + (err_code(kv.get("err")) or "")))

        elif msg == PROACTIVE_TRY:
            g = group_key(kv.get("group"))
            if is_selftest(g):
                continue
            stats[g]["proactive_try"] += 1
            timeline[g].append((ts, ROLE_BOT, "（冷场主动找话）", kv.get("冷场") or ""))

        elif msg == DEDUP:
            g = group_key(kv.get("session"))
            if is_selftest(g):
                continue
            stats[g]["dedup"] += 1
            timeline[g].append((ts, ROLE_BOT, kv.get("text") or "", "重复发言被拦下"))

        elif msg == SUMMARY:
            g = group_key(kv.get("group"))
            if is_selftest(g):
                continue
            timeline[g].append((ts, "系统", f"前文提要已更新（{kv.get('长度')} 字）", ""))

        elif msg == BOOT:
            boots.append(ts)
        elif msg == CFG_SAVED:
            system_events.append((ts, "配置", "配置已写入 " + str(kv.get("path"))))
        elif msg in (MASTER_BOUND, MASTER_BOUND_ADMIN):
            system_events.append((ts, "认主", f"{kv.get('name')} 绑定为主人"))
        elif msg == ENV_REFRESH:
            system_events.append((ts, "环境", f"{kv.get('day')} · {kv.get('special')} · {kv.get('weather')}"))
        elif msg == SCHEDULE:
            system_events.append((ts, "在线时段", f"档位 {kv.get('档位')} · {kv.get('当前时段')} · 在线率 {kv.get('在线率')}"))
        elif msg in (CALL_FAIL, CALL_RETRY):
            system_events.append((ts, "模型调用", msg + " " + json.dumps(kv, ensure_ascii=False)[:200]))
        elif msg in (PARSE_FAIL, SIGN_FAIL):
            system_events.append((ts, "平台", msg + " " + json.dumps(kv, ensure_ascii=False)[:160]))

    def esc(s):
        return redact(s).replace("|", "\\|").replace("\n", " ").strip()

    def hhmmss(ts):
        return ts[5:19] if len(ts) >= 19 else ts  # MM-DD HH:MM:SS，跨天也不会看混

    # ---- 输出 ----
    out = []
    w = out.append
    w("# 群聊对话记录（%s ~ %s）" % (t0[:16], t1[:16]))
    w("")
    w("> 本文档由 `scripts/export_chatlog.py` 从 systemd 日志自动导出，")
    w("> 用途是给**行为分析智能体**当语料：看它什么时候说话、说了什么、什么时候沉默、哪里出错。")
    w("> 每条都带时间戳；机器人自己的发言按「拆句发送」的原样保留（一条消息内部用 ` / ` 分隔）。")
    w("")
    w("## 读这份文档前必须知道的三件事")
    w("")
    w("1. **收到的消息被截断到 60 字**（日志里 `truncate(content, 60)`，超出以 `…` 结尾），长消息只有开头。")
    w("2. **沉默没有记录**：日志只记「发生了什么」。没回话的轮次不会留下任何痕迹，")
    w("   只能通过「收到 N 条 / 只回了 K 条」反推参与度。")
    w("3. **@ 是平台渲染的**：被动回复会被客户端渲染成「@原发送者」，程序端去不掉，")
    w("   所以「每条都艾特」不是机器人的措辞问题，是被动回复通道的固有表现。")
    w("")
    w("## 总览")
    w("")
    w("| 群 | openid | 收到 | 其中@机器人 | 成功发言(次) | 发出条数 | 发送失败 | 主动找话(次) | 重复被拦 |")
    w("|---|---|---:|---:|---:|---:|---:|---:|---:|")
    order = sorted(timeline, key=lambda g: timeline[g][0][0])
    for g in order:
        s = stats[g]
        fails = s["fail"]
        w("| %s | `%s` | %d | %d | %d | %d | %d | %d | %d |" % (
            esc(group_label(g)), g, s["in"], s["at"], s["spoke"], s["segs"],
            fails, s["proactive_try"], s["dedup"]))
    w("")
    w("- 服务重启 %d 次（每次重启后短期记忆清零，长期记忆读盘恢复）。" % len(boots))
    w("- 私聊 %d 条（见文末）。" % len(c2c_timeline))
    w("")

    # ---- 每个群一节 ----
    for idx, g in enumerate(order, 1):
        mg = mem_groups.get(g, {})
        label = group_label(g)
        s = stats[g]
        w("## %d. 群「%s」 · `%s`" % (idx, esc(label), g))
        w("")
        # 成员
        members = mg.get("members") or {}
        if members:
            w("**成员**（来自长期记忆，`msg_count` 是累计发言数）")
            w("")
            w("| 昵称 | openid | 累计发言 | 最近发言 | 主人 |")
            w("|---|---|---:|---|---|")
            for m in sorted(members.values(), key=lambda x: -(x.get("msg_count") or 0)):
                w("| %s | `%s` | %d | %s | %s |" % (
                    esc(m.get("name") or short_id(m.get("openid", ""))),
                    m.get("openid", ""), m.get("msg_count") or 0,
                    (m.get("last_seen") or "")[:19].replace("T", " "),
                    "是" if m.get("is_master") else ""))
            w("")
        facts = mg.get("facts") or {}
        if facts:
            w("**它记住的要点**")
            w("")
            for k, v in facts.items():
                w("- %s：%s" % (esc(k), esc(v)))
            w("")
        ratio = "%.0f%%" % (100.0 * s["spoke"] / s["in"]) if s["in"] else "—"
        w("**参与度**：收到 %d 条 → 成功发言 %d 次（粗算回复率 %s）；@ 机器人 %d 条；发送失败 %d 条。"
          % (s["in"], s["spoke"], ratio, s["at"], s["fail"]))
        if s["at"] == s["in"] and s["in"] > 3:
            w("")
            w("> ⚠️ 这个群**所有**收到的消息都是 @ 机器人的，说明该群未开启「全量消息」，")
            w("> 平台只推送 @ 消息 → 它无法主动参与闲聊，且每条回复都会被渲染成 @。")
        w("")
        w("**对话时间线**")
        w("")
        w("| 时间 | 说话人 | 内容 | 备注 |")
        w("|---|---|---|---|")
        for ts, who, text, note in timeline[g]:
            w("| %s | %s | %s | %s |" % (hhmmss(ts), esc(who), esc(text), esc(note)))
        w("")

    # ---- 私聊 ----
    w("## 私聊（C2C）")
    w("")
    w("| 时间 | 说话人 | 内容 | 备注 |")
    w("|---|---|---|---|")
    for ts, who, text, note in c2c_timeline:
        w("| %s | %s | %s | %s |" % (hhmmss(ts), esc(who), esc(text), esc(note)))
    w("")

    # ---- 附录 ----
    w("## 附录 A · 系统与环境事件")
    w("")
    w("| 时间 | 类别 | 事件 |")
    w("|---|---|---|")
    for ts, cat, txt in sorted(system_events):
        w("| %s | %s | %s |" % (hhmmss(ts), cat, esc(txt)))
    w("")
    w("## 附录 B · 服务重启时间点")
    w("")
    for ts in boots:
        w("- %s" % ts)
    w("")
    w("## 附录 C · 字段与口径说明")
    w("")
    w("- `收到`：日志 `群消息`，平台推给机器人的进群消息（未开全量消息的群只有 @ 消息会被推）。")
    w("- `成功发言`：日志 `已发言`，一次决策可能产生多条（拆句发送），`条数` 就是拆了几条。")
    w("- `发送失败`：日志 `发言发送失败`，常见 `40034105`（主动消息无权限）。")
    w("- `主动找话`：巡检发现冷场后尝试主动发言（需要平台主动消息权限，目前没有）。")
    w("- `重复被拦`：去重命中，说明它想说的话和刚说过的一模一样。")
    w("- 空内容：`群消息` 里 text 为空通常是纯图片/表情包，或只 @ 了一下没说话。")
    w("")

    os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
    with open(args.out, "w", encoding="utf-8") as f:
        f.write("\n".join(out) + "\n")
    print("已写出 %s（%d 群，%d 行时间线）" % (
        args.out, len(order), sum(len(v) for v in timeline.values())))


if __name__ == "__main__":
    main()
