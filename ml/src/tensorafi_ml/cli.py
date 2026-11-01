"""tensora-ml command line. Results are JSON on stdout; a failed check exits 1."""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
from pathlib import Path
from typing import Any

GIB = 1024**3


def _emit(result: Any) -> None:
    json.dump(result, sys.stdout, indent=2, default=str)
    sys.stdout.write("\n")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="tensora-ml", description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("validate", help="check a local model directory against the model policy")
    p.add_argument("path", type=Path)
    p.add_argument("--max-gb", type=float, default=200)

    p = sub.add_parser("ingest", help="download hf://org/model[@rev], validate, upload to s3://")
    p.add_argument("source")
    p.add_argument("dest")
    p.add_argument("--max-gb", type=float, default=200)

    p = sub.add_parser("bench", help="load-test an OpenAI-compatible endpoint")
    p.add_argument("base_url")
    p.add_argument("--model", required=True)
    p.add_argument("--requests", type=int, default=64)
    p.add_argument("--concurrency", type=int, default=8)
    p.add_argument("--max-tokens", type=int, default=256)

    p = sub.add_parser("burnin", help="stress every visible GPU and check for errors")
    p.add_argument("--seconds", type=float, default=300)
    p.add_argument("--matrix-size", type=int, default=8192)

    args = parser.parse_args(argv)

    if args.command == "validate":
        from tensora_ml.validate import validate_model_dir

        report = validate_model_dir(args.path, int(args.max_gb * GIB))
        _emit(report.as_dict())
        return 0 if report.ok else 1

    if args.command == "ingest":
        from tensora_ml.ingest import ModelRejectedError, ingest

        try:
            _emit(ingest(args.source, args.dest, max_total_bytes=int(args.max_gb * GIB)))
        except ModelRejectedError as exc:
            _emit(exc.report.as_dict())
            return 1
        return 0

    if args.command == "bench":
        from tensora_ml import bench

        _emit(
            asyncio.run(
                bench.run(
                    args.base_url,
                    args.model,
                    requests=args.requests,
                    concurrency=args.concurrency,
                    max_tokens=args.max_tokens,
                    api_key=os.environ.get("TENSORA_BENCH_API_KEY"),
                )
            )
        )
        return 0

    if args.command == "burnin":
        from tensora_ml import burnin

        results = burnin.run(seconds=args.seconds, matrix_size=args.matrix_size)
        _emit(results)
        return 0 if all(r["passed"] for r in results) else 1

    parser.error(f"unknown command {args.command}")
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
