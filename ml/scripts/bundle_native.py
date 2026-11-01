"""Build the deterministic Python source bundle embedded in the native and Docker drivers."""

from pathlib import Path
from zipfile import ZIP_DEFLATED, ZipFile, ZipInfo

root = Path(__file__).resolve().parents[2]
source = root / "ml/src"
target = root / "controlplane/internal/agent/drivers/pythonruntime/runtime.zip"
with ZipFile(target, "w", compression=ZIP_DEFLATED) as archive:
    for path in sorted((source / "einsora_ml").glob("*.py")):
        entry = ZipInfo(path.relative_to(source).as_posix(), date_time=(1980, 1, 1, 0, 0, 0))
        entry.compress_type = ZIP_DEFLATED
        entry.external_attr = 0o100644 << 16
        archive.writestr(entry, path.read_bytes())
print(f"Bundled einsora_ml sources into {target.relative_to(root)}")
