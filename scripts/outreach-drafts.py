#!/usr/bin/env python3
"""Day 30: turn your target list into outreach DRAFTS you finish by hand. It never writes the first line.

    scripts/outreach-drafts.py TARGETS.xlsx --video URL --n 30 > drafts.md
    scripts/outreach-drafts.py TARGETS.csv  --video URL --ids 1,2,5   > drafts.md
    scripts/outreach-drafts.py --check drafts.md                      # fails while any placeholder is left

The roadmap's rule: always personalize the first line, never mass-paste, ask for a conversation first,
follow up once after 4 or 5 days. So each draft has a placeholder where YOUR specific first line goes, the
target's own notes from your list (what they do, the angle) so you can write it fast, and the
"how sure I am" column, so you verify a company before you message it. --check refuses to pass while
a placeholder remains. Reads .xlsx (needs openpyxl: pip install openpyxl) or .csv (no install).
"""
import argparse, csv, re, sys

PH = "⟦FIRST LINE: one specific thing you saw⟧"
PH_VIDEO = "⟦VIDEO LINK⟧"

BODY = (
    "I built an open-source AI agent runtime that keeps working with the internet off (local model, CRDT sync, "
    "thousands of random failure schedules tested; benchmarks in the README). 90-second demo: {video}. "
    "If offline reliability is something your team deals with, I'd love 15 minutes to hear how you handle it. "
    "I'm also open to roles."
)

def rows_from(path):
    if path.lower().endswith(".csv"):
        with open(path, newline="", encoding="utf-8") as f:
            return list(csv.DictReader(f))
    try:
        import openpyxl
    except ImportError:
        sys.exit("reading .xlsx needs openpyxl (pip install openpyxl), or export the Targets sheet as CSV")
    wb = openpyxl.load_workbook(path, read_only=True, data_only=True)
    ws = wb["Targets"] if "Targets" in wb.sheetnames else wb.worksheets[0]
    it = ws.iter_rows(values_only=True)
    head = [str(h or "").strip() for h in next(it)]
    return [dict(zip(head, r)) for r in it if any(r)]

def check(path):
    text = open(path, encoding="utf-8").read()
    left = text.count("⟦")
    drafts = len(re.findall(r"^### ", text, re.M))
    if left:
        print(f"{left} placeholder(s) left in {drafts} drafts: write the first line of each, add the video link")
        return 1
    print(f"ok: {drafts} drafts, no placeholders left")
    return 0

def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file", nargs="?")
    ap.add_argument("--video", default=PH_VIDEO)
    ap.add_argument("--n", type=int, default=30, help="how many, from the top of the list (default 30)")
    ap.add_argument("--ids", help="comma-separated # values to include instead of the first N")
    ap.add_argument("--check", metavar="DRAFTS", help="fail while a placeholder is left in this file")
    a = ap.parse_args()
    if a.check:
        sys.exit(check(a.check))
    if not a.file:
        ap.error("give the target list file")
    rows = rows_from(a.file)
    if a.ids:
        want = {s.strip() for s in a.ids.split(",")}
        rows = [r for r in rows if str(r.get("#")) in want]
    else:
        rows = rows[: a.n]
    out = ["# Outreach drafts", "",
           "Write the first line of every draft yourself, from something you actually read or watched. "
           "Verify anything marked *verify* on the company's own site first. Send one at a time, not in a batch. "
           "Follow up once after 4 or 5 days, with something new, not a nudge.", ""]
    for r in rows:
        sure = str(r.get("How sure I am") or "")
        out += [f"### {r.get('#')}. {r.get('Name')}  ({r.get('Category')})", "",
                f"- Site: {r.get('Website')}",
                f"- What they do: {r.get('What they do (specific)')}",
                f"- Angle: {r.get('Why Offgrid is relevant / angle for a comment')}",
                f"- Confidence in these notes: {sure}" + ("  **VERIFY FIRST**" if "erify" in sure else ""),
                "", f"> Hi ⟦NAME⟧, {PH}", ">", "> " + BODY.format(video=a.video), ""]
    print("\n".join(out))

if __name__ == "__main__":
    main()
