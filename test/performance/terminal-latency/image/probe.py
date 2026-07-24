#!/usr/bin/env python3
"""Deterministic raw-terminal peer for the Discobox latency harness."""

import os
import re
import termios
import threading
import time
import tty


PING = re.compile(rb"^DBXPING:(\d{8})$")
PROFILES = {"quiet", "spinner", "screen"}


def positive_int(name: str, fallback: int) -> int:
    raw = os.environ.get(name, str(fallback))
    try:
        value = int(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be an integer") from exc
    if value < 1:
        raise ValueError(f"{name} must be positive")
    return value


def output_frame(profile: str, sequence: int, frame_bytes: int) -> bytes:
    if profile == "spinner":
        prefix = (
            f"\r\x1b[2KDISCOBOX_LATENCY_READY latency spinner {sequence:08d} "
        ).encode()
    else:
        prefix = (
            f"\x1b[HDISCOBOX_LATENCY_READY latency full-screen frame {sequence:08d}\r\n"
        ).encode()
    if len(prefix) >= frame_bytes:
        return prefix[:frame_bytes]
    return prefix + (b"." * (frame_bytes - len(prefix)))


def write_output_load(stop: threading.Event, profile: str, hz: int, frame_bytes: int) -> None:
    interval = 1.0 / hz
    deadline = time.monotonic()
    sequence = 0
    while not stop.is_set():
        os.write(1, output_frame(profile, sequence, frame_bytes))
        sequence += 1
        deadline += interval
        remaining = deadline - time.monotonic()
        if remaining > 0:
            stop.wait(remaining)
        else:
            # Do not accumulate a burst after the process was descheduled.
            deadline = time.monotonic()


def main() -> None:
    stdin_fd = 0
    stdout_fd = 1
    profile = os.environ.get("DISCOBOX_LATENCY_OUTPUT_PROFILE", "quiet").strip()
    if profile not in PROFILES:
        raise ValueError(
            "DISCOBOX_LATENCY_OUTPUT_PROFILE must be quiet, spinner, or screen"
        )
    if profile == "quiet":
        hz = 0
        frame_bytes = 0
    else:
        default_bytes = 128 if profile == "spinner" else 4800
        hz = positive_int("DISCOBOX_LATENCY_OUTPUT_HZ", 30)
        frame_bytes = positive_int("DISCOBOX_LATENCY_OUTPUT_BYTES", default_bytes)

    previous = termios.tcgetattr(stdin_fd)
    tty.setraw(stdin_fd, termios.TCSANOW)
    stop = threading.Event()
    writer = None
    try:
        os.write(
            stdout_fd,
            f"DISCOBOX_LATENCY_LOAD:{profile}:{hz}:{frame_bytes}\r\n".encode(),
        )
        os.write(stdout_fd, b"DISCOBOX_LATENCY_READY\r\n")
        if profile != "quiet":
            writer = threading.Thread(
                target=write_output_load,
                args=(stop, profile, hz, frame_bytes),
                daemon=True,
            )
            writer.start()
        pending = bytearray()
        while True:
            chunk = os.read(stdin_fd, 4096)
            if not chunk:
                return
            for value in chunk:
                if value not in (10, 13):
                    pending.append(value)
                    continue
                if not pending:
                    continue
                match = PING.match(bytes(pending))
                pending.clear()
                if match is not None:
                    os.write(stdout_fd, b"DBXPONG:" + match.group(1) + b"\r\n")
    finally:
        stop.set()
        if writer is not None:
            writer.join(timeout=1)
        termios.tcsetattr(stdin_fd, termios.TCSANOW, previous)


if __name__ == "__main__":
    main()
