import os
from PIL import Image, ImageDraw, ImageFont

# Terminal text lines with styling tags
raw_text = """rebik@rebik-HP-EliteBook-845-G8-Notebook-PC:~/Documents/Go_Projects/offgrid$ test -z "$(gofmt -l .)" && echo "==> gofmt: PASS" && \\
go vet ./... && echo "==> go vet: PASS" && \\
make wasm-test && \\
go test -v -race ./internal/phone ./cmd/offgrid -run "TestPhone|TestDay35"
==> gofmt: PASS
==> go vet: PASS
scripts/wasm-check.sh --run
== imports of ./internal/crdt ./internal/hlc
ok: standard library only
== build for js/wasm: ./internal/crdt ./internal/hlc
ok: builds
== test as wasm: ./internal/crdt
PASS
== test as wasm: ./internal/hlc
PASS
ok: tests pass under WebAssembly
=== RUN   TestPhoneClientEndToEnd
--- PASS: TestPhoneClientEndToEnd (0.07s)
PASS
ok      offgrid/internal/phone  1.088s
=== RUN   TestDay35ThreatModelV2Completeness
--- PASS: TestDay35ThreatModelV2Completeness (0.00s)
=== RUN   TestDay35ADRsCompleteAndReversible
--- PASS: TestDay35ADRsCompleteAndReversible (0.00s)
=== RUN   TestDay35NoStrayTODOCommentsInProductionCode
--- PASS: TestDay35NoStrayTODOCommentsInProductionCode (0.02s)
=== RUN   TestDay35CLIExecCreatesPrivateOwnerOnlyDatabase
--- PASS: TestDay35CLIExecCreatesPrivateOwnerOnlyDatabase (0.03s)
=== RUN   TestPhoneCLIUsage
--- PASS: TestPhoneCLIUsage (0.00s)
=== RUN   TestPhoneCLINoDatabase
--- PASS: TestPhoneCLINoDatabase (0.00s)
=== RUN   TestPhoneCLIPairListAndRevoke
--- PASS: TestPhoneCLIPairListAndRevoke (0.04s)
=== RUN   TestPhoneCLIServeSmoke
--- PASS: TestPhoneCLIServeSmoke (1.47s)
PASS
ok      offgrid/cmd/offgrid     2.605s
rebik@rebik-HP-EliteBook-845-G8-Notebook-PC:~/Documents/Go_Projects/offgrid$ """

font_bold_path = "/usr/share/fonts/truetype/jetbrains-mono-zorin-os/JetBrainsMono-Bold.ttf"
font_reg_path = "/usr/share/fonts/truetype/jetbrains-mono-zorin-os/JetBrainsMono-Regular.ttf"

font_size = 20
line_height = 29
font_bold = ImageFont.truetype(font_bold_path, font_size)
font_reg = ImageFont.truetype(font_reg_path, font_size)

lines = raw_text.strip().split("\n")

# Dimensions
padding_x = 36
padding_y = 28
header_height = 46

content_height = len(lines) * line_height
img_width = 1140
img_height = header_height + content_height + padding_y * 2

img = Image.new("RGBA", (img_width, img_height), (13, 17, 23, 255))
draw = ImageDraw.Draw(img)

# Window Header
draw.rectangle([(0, 0), (img_width, header_height)], fill=(22, 27, 34, 255))
draw.line([(0, header_height), (img_width, header_height)], fill=(48, 54, 61, 255), width=1)

# Window Controls (macOS style dots)
dots = [
    (24, 23, (248, 81, 73)),
    (46, 23, (210, 153, 34)),
    (68, 23, (46, 160, 67)),
]
for x, y, color in dots:
    draw.ellipse([(x - 6, y - 6), (x + 6, y + 6)], fill=color)

# Header Title
title = "rebik@rebik-HP-EliteBook-845-G8-Notebook-PC: ~/Documents/Go_Projects/offgrid"
title_font = ImageFont.truetype(font_reg_path, 13)
bbox = title_font.getbbox(title)
title_w = bbox[2] - bbox[0]
draw.text(((img_width - title_w) // 2, 14), title, fill=(139, 148, 158), font=title_font)

# Color constants
C_PROMPT_USER = (74, 246, 38)     # Vibrant Green
C_PROMPT_DIR  = (56, 189, 248)    # Sky Blue
C_TEXT_WHITE  = (241, 245, 249)   # Crisp White
C_PASS_GREEN  = (34, 197, 94)     # Emerald Green
C_RUN_MUTED   = (148, 163, 184)   # Slate Muted
C_PKG_OK      = (52, 211, 153)    # Mint Green

curr_y = header_height + padding_y

def draw_prompt(y, command=""):
    user_part = "rebik@rebik-HP-EliteBook-845-G8-Notebook-PC"
    sep_part = ":"
    dir_part = "~/Documents/Go_Projects/offgrid"
    end_part = "$ "
    
    x = padding_x
    draw.text((x, y), user_part, fill=C_PROMPT_USER, font=font_bold)
    x += font_bold.getbbox(user_part)[2]
    
    draw.text((x, y), sep_part, fill=C_TEXT_WHITE, font=font_bold)
    x += font_bold.getbbox(sep_part)[2]
    
    draw.text((x, y), dir_part, fill=C_PROMPT_DIR, font=font_bold)
    x += font_bold.getbbox(dir_part)[2]
    
    draw.text((x, y), end_part, fill=C_TEXT_WHITE, font=font_bold)
    x += font_bold.getbbox(end_part)[2]
    
    if command:
        draw.text((x, y), command, fill=C_TEXT_WHITE, font=font_bold)

for i, line in enumerate(lines):
    if line.startswith("rebik@rebik-HP-EliteBook-845-G8-Notebook-PC:~/Documents/Go_Projects/offgrid$ "):
        cmd = line.replace("rebik@rebik-HP-EliteBook-845-G8-Notebook-PC:~/Documents/Go_Projects/offgrid$ ", "")
        draw_prompt(curr_y, cmd)
    elif line.strip() == "rebik@rebik-HP-EliteBook-845-G8-Notebook-PC:~/Documents/Go_Projects/offgrid$":
        draw_prompt(curr_y, "")
    elif "PASS" in line:
        if line.strip() == "PASS":
            draw.text((padding_x, curr_y), line, fill=C_PASS_GREEN, font=font_bold)
        elif "--- PASS:" in line:
            draw.text((padding_x, curr_y), "--- PASS: ", fill=C_PASS_GREEN, font=font_bold)
            rest = line.replace("--- PASS: ", "")
            w = font_bold.getbbox("--- PASS: ")[2]
            draw.text((padding_x + w, curr_y), rest, fill=C_TEXT_WHITE, font=font_bold)
        elif "==> " in line and ": PASS" in line:
            parts = line.split(": PASS")
            draw.text((padding_x, curr_y), parts[0] + ": ", fill=C_TEXT_WHITE, font=font_bold)
            w = font_bold.getbbox(parts[0] + ": ")[2]
            draw.text((padding_x + w, curr_y), "PASS", fill=C_PASS_GREEN, font=font_bold)
        else:
            draw.text((padding_x, curr_y), line, fill=C_PASS_GREEN, font=font_bold)
    elif "=== RUN" in line:
        draw.text((padding_x, curr_y), line, fill=C_RUN_MUTED, font=font_reg)
    elif line.startswith("ok      ") or line.startswith("ok: "):
        draw.text((padding_x, curr_y), line[:3], fill=C_PKG_OK, font=font_bold)
        w = font_bold.getbbox(line[:3])[2]
        draw.text((padding_x + w, curr_y), line[3:], fill=C_TEXT_WHITE, font=font_bold)
    elif line.startswith("=="):
        draw.text((padding_x, curr_y), line, fill=(210, 153, 34), font=font_bold)
    else:
        draw.text((padding_x, curr_y), line, fill=C_TEXT_WHITE, font=font_reg)
    
    curr_y += line_height

# Save to destination
out_path = "docs/day103-phone-client-terminal.png"
os.makedirs("docs", exist_ok=True)
img.save(out_path, "PNG")
print(f"Generated high-contrast terminal screenshot: {out_path}")
