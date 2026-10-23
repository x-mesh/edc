#!/usr/bin/env python3
import argparse
import json
import os
import shlex
from pathlib import Path
import subprocess
import tempfile

parser = argparse.ArgumentParser(description="Render README terminal captures from demo data")
parser.add_argument("screens", nargs="*", choices=["top", "info", "trace-http", "history", "disk-usage", "clean"])
arguments = parser.parse_args()

root = Path(__file__).resolve().parents[2]
with tempfile.TemporaryDirectory(prefix="edc-capture-") as temporary:
    work = Path(temporary)
    overlay = work / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {
        str(root / "internal/edc/documentation_capture_test.go"):
        str(root / "docs/capture/frames.go.txt")
    }}))
    environment = dict(os.environ, EDC_CAPTURE_DIR=str(work), EDC_LANG="en", XDG_CONFIG_HOME=str(work / "config"), FORCE_COLOR="1", CLICOLOR_FORCE="1")
    environment.pop("NO_COLOR", None)
    subprocess.run(["go", "test", "-overlay", str(overlay), "./internal/edc", "-run", "^TestDocumentationFrames$", "-count=1"], cwd=root, env=environment, check=True)
    for command, height in [("top", 930), ("info", 1250), ("trace-http", 930), ("history", 800), ("disk-usage", 500), ("clean", 760)]:
        if arguments.screens and command not in arguments.screens:
            continue
        display = {"trace-http": "trace http --side server --port 8080", "history": "history trend", "disk-usage": "du /var"}.get(command, command)
        preparation = f"export PS1='$ '; function edc() {{ cat {shlex.quote(str(work / (command + '.ansi')))}; PS1=''; tput civis; }}; clear"
        tape = work / f"{command}.tape"
        tape.write_text(f'''Source docs/tape/_settings.tape
Set Width 1520
Set Height {height}
Hide
Set TypingSpeed 1ms
Type {json.dumps(preparation)}
Enter
Show
Set TypingSpeed 55ms
Type "edc {display}"
Enter
Sleep 1s
Screenshot {json.dumps(str(work / (command + ".png")))}
Sleep 500ms
''')
        subprocess.run(["vhs", str(tape)], cwd=root, env=environment, check=True)
        (root / "docs/media" / f"{command}.png").write_bytes((work / f"{command}.png").read_bytes())
