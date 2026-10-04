import json, sys, io

PATH = "/root/dev-work/golang-dev/dadyumo-qqbot/data/memory.json"

# 2026-10-04 事故：上游以 HTTP 200 + 正文的形式拒绝了请求，那句含
# "sensitive words" 的英文被当成发言发进群，又被写回记忆，
# 于是每轮上下文都带着它 → 再次触发拒绝 → 再次写回，自我复制。
# 这里把这类行删掉，断掉循环。
BAD = [
    "could not be submitted",
    "sensitive words",
    # 存进记忆时这句被截断过（末尾是单个省略号字符），所以按前缀匹配而不是整句
    "Prohibited U",
    "Generative AI",
    "violate Google's",
]

with io.open(PATH, encoding="utf-8") as f:
    d = json.load(f)

removed = 0
for g in d.get("groups", []):
    recent = g.get("recent") or []
    keep = []
    for line in recent:
        c = str(line.get("c", ""))
        if any(b in c for b in BAD):
            removed += 1
            continue
        keep.append(line)
    g["recent"] = keep

if removed == 0:
    print("no poison found, nothing changed")
    sys.exit(0)

with io.open(PATH, "w", encoding="utf-8") as f:
    json.dump(d, f, ensure_ascii=False, indent=2)

print("removed lines:", removed)
for g in d.get("groups", []):
    n = len(g.get("recent") or [])
    poison = sum(1 for l in (g.get("recent") or [])
                 if any(b in str(l.get("c", "")) for b in BAD))
    print("  group=%s recent=%d poison=%d" % (g.get("name", "?"), n, poison))