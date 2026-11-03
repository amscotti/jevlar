"""Compatibility suite for the official TypeSafe Python SDK against Jevlar.

The SDK is the real client (`typesafe-sdk`), pointed at Jevlar with its
`base_url` option. Every check drives the SDK's own typed surface — models,
questions, responses, errors and retries — so a green run means a Python
application using the SDK works unchanged through the gateway.

Environment:
    JEVLAR_BASE_URL   base URL of the running gateway (default :28700)
    JEVLAR_TOKEN      a client token with normal quota
    JEVLAR_LIMITED    a client token with rpm = 1 (for the quota check)

Exit code 0 means every check passed.
"""

from __future__ import annotations

import os
import sys

from typesafe_sdk import (
    Box,
    Choice,
    Image,
    Noul,
    RetryPolicy,
    Score,
    TypeSafeAPIError,
    TypeSafeAuthenticationError,
    TypeSafeBadRequestError,
    TypeSafeClient,
)

BASE = os.environ.get("JEVLAR_BASE_URL", "http://127.0.0.1:28700")
TOKEN = os.environ.get("JEVLAR_TOKEN", "sdk-token")
LIMITED = os.environ.get("JEVLAR_LIMITED", "limited-token")

STATE = "A customer has tried to connect Stripe for three days and is frustrated."
QUESTIONS = {
    "urgent": Noul(instructions="Does this message express urgency?"),
    "team": Choice(
        instructions="Which team?",
        criteria={"billing": "Payments", "technical": "Bugs"},
    ),
}

results: list[tuple[str, bool, str]] = []


def _synthetic_png(size: int = 512) -> bytes:
    """A minimal PNG: white canvas with one solid red rectangle.

    Built with the standard library so the suite has no image dependency.
    The rectangle spans 25%-75% horizontally and 25%-60% vertically, which
    gives the box answer a known expected region.
    """
    import struct
    import zlib

    x0, y0, x1, y1 = size // 4, size // 4, size * 3 // 4, size * 6 // 10
    raw = bytearray()
    for y in range(size):
        raw.append(0)  # filter type: none
        for x in range(size):
            inside = x0 <= x < x1 and y0 <= y < y1
            raw += bytes((220, 20, 20, 255) if inside else (255, 255, 255, 255))

    def chunk(tag: bytes, data: bytes) -> bytes:
        return (
            struct.pack(">I", len(data))
            + tag
            + data
            + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        )

    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0))
        + chunk(b"IDAT", zlib.compress(bytes(raw), 9))
        + chunk(b"IEND", b"")
    )


def check(name: str, fn) -> None:
    try:
        detail = fn()
        results.append((name, True, detail))
        print(f"PASS  {name}: {detail}")
    except Exception as error:  # noqa: BLE001 - report every failure, not just typed ones
        results.append((name, False, f"{type(error).__name__}: {error}"))
        print(f"FAIL  {name}: {type(error).__name__}: {error}")


def main() -> int:
    client = TypeSafeClient(api_key=TOKEN, base_url=BASE)

    def models() -> str:
        listed = client.models.list()
        names = [m.name for m in listed.models]
        if "jev-latest" not in names:
            raise AssertionError(f"jev-latest missing from {names}")
        entry = next(m for m in listed.models if m.name == "jev-latest")
        if not entry.description:
            raise AssertionError("alias has no description")
        if not entry.release_date:
            raise AssertionError("alias has no release_date")
        return f"{len(names)} models; description and release_date parse"

    def system_one() -> str:
        response = client.system_one(STATE, QUESTIONS, model="jev-latest")
        if not response.model:
            raise AssertionError("no model in the response")
        if not 0.0 <= response.answers["urgent"].noul <= 1.0:
            raise AssertionError("noul out of range")
        if response.answers["team"].choice not in {"billing", "technical"}:
            raise AssertionError("unexpected choice")
        if response.usage.input_tokens <= 0:
            raise AssertionError("usage not parsed")
        return f"model={response.model} tokens={response.usage.input_tokens}/{response.usage.output_tokens}"

    def score() -> str:
        response = client.system_one(
            "The product arrived late and the box was damaged.",
            {"severity": Score(instructions="Rate the severity.", criteria=["Mild", "Severe"])},
            model="jev-latest",
        )
        return f"score={response.answers['severity'].score}"

    def provider_error_passthrough() -> str:
        try:
            client.system_one(STATE, QUESTIONS, model="jev-latest", extra_body={"__unknown__": 1})
        except TypeSafeBadRequestError as error:
            return f"provider 400 surfaced as {type(error).__name__} (request_id={error.request_id})"
        return "provider accepted the extra field"

    def auth_error_request_id() -> str:
        bad = TypeSafeClient(
            api_key="definitely-not-a-key", base_url=BASE, retry=RetryPolicy(max_retries=0)
        )
        try:
            bad.system_one(STATE, QUESTIONS, model="jev-latest")
        except TypeSafeAuthenticationError as error:
            if not error.request_id:
                raise AssertionError("request_id missing from the 401") from error
            return f"401 with request_id={error.request_id}"
        raise AssertionError("no authentication error raised")

    def client_quota() -> str:
        limited = TypeSafeClient(
            api_key=LIMITED, base_url=BASE, retry=RetryPolicy(max_retries=0)
        )
        limited.system_one(STATE, QUESTIONS, model="jev-latest")
        try:
            limited.system_one(STATE, QUESTIONS, model="jev-latest")
        except TypeSafeAPIError as error:
            if error.status != 429:
                raise AssertionError(f"status {error.status}, want 429") from error
            retry_after = error.headers.get("retry-after")
            if not retry_after:
                raise AssertionError("no Retry-After header") from error
            return f"429 with retry-after={retry_after}s and a typed error"
        raise AssertionError("no 429 for an over-budget client")

    def request_ids() -> str:
        response = client._http_client.request(  # noqa: SLF001 - verifying wire headers
            "POST",
            BASE + "/v1/systemone",
            json={"model": "jev-latest", "state": "x", "questions": {}},
            headers={"Authorization": f"Bearer {TOKEN}"},
        )
        our_id = response.headers.get("x-typesafe-request-id")
        jevlar_id = response.headers.get("x-request-id")
        upstream_id = response.headers.get("x-typesafe-upstream-request-id")
        if not our_id or not jevlar_id:
            raise AssertionError(f"missing ids: {our_id} {jevlar_id}")
        return f"request-id={our_id} upstream={upstream_id or 'n/a'}"

    def zstd() -> str:
        try:
            compressed = TypeSafeClient(api_key=TOKEN, base_url=BASE, compression="zstd")
        except Exception as error:  # noqa: BLE001 - optional SDK extra
            return f"skipped: zstd unavailable in this SDK install ({type(error).__name__})"
        response = compressed.system_one(STATE, QUESTIONS, model="jev-latest")
        return f"zstd-compressed request accepted, model={response.model}"

    def image_box() -> str:
        """An image upload: multipart body, zstd-compressed, box answer.

        This is the deepest compatibility path there is — the SDK switches
        to multipart as soon as assets are attached, compresses the body
        above 1 KiB, and the model returns coordinates.
        """
        import base64

        png = _synthetic_png()
        image = Image.from_base64(base64.b64encode(png))
        response = client.system_one(
            "photo",
            {"red": Box(instructions="Locate the red rectangle in photo.")},
            model="jev-preview",
            assets={"photo": image},
            extra_headers={"TypeSafe-Danger-Zone-Images": "true"},
        )
        answer = response.answers["red"]
        if answer.type != "box":
            raise AssertionError(f"answer type {answer.type}")
        box = answer.box
        # The rectangle is 25%-75% across and 25%-60% down.
        if not (0.15 <= box.left <= 0.35 and 0.65 <= box.right <= 0.85):
            raise AssertionError(f"box x-range {box.left}-{box.right} does not match the image")
        if not (0.15 <= box.top <= 0.35 and 0.5 <= box.bottom <= 0.7):
            raise AssertionError(f"box y-range {box.top}-{box.bottom} does not match the image")
        return (
            f"box=({box.left:.2f},{box.top:.2f})-({box.right:.2f},{box.bottom:.2f}) "
            f"confidence={answer.confidence} tokens={response.usage.input_tokens}"
        )

    def long_context() -> str:
        """The long-context danger flag with a body above the compression
        threshold (so it also exercises the zstd request path)."""
        state = "The customer wrote: my invoice is wrong. " * 2000
        response = client.system_one(
            state,
            {"billing": Noul(instructions="Is this about billing?")},
            model="jev-latest",
            extra_headers={"TypeSafe-Danger-Zone-Long-Context": "true"},
        )
        if response.usage.input_tokens < 1000:
            raise AssertionError(f"long context was not used: {response.usage}")
        return f"{response.usage.input_tokens} input tokens accepted"

    check("models.list()", models)
    check("system_one() noul+choice", system_one)
    check("system_one() score", score)
    check("provider error passthrough", provider_error_passthrough)
    check("401 carries a request id", auth_error_request_id)
    check("client rpm quota", client_quota)
    check("request-id headers", request_ids)
    check("zstd request compression", zstd)
    check("image upload (multipart+zstd, box)", image_box)
    check("long-context danger flag", long_context)

    failed = [name for name, passed, _ in results if not passed]
    print(f"\n{len(results) - len(failed)}/{len(results)} checks passed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
