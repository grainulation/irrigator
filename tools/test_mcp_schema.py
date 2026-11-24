"""Bounded discovery must time out even after a server emits partial JSON."""
import importlib.util
import os
import pathlib
import select
import subprocess
import sys
import time
import unittest

spec = importlib.util.spec_from_file_location("mcp_schema", pathlib.Path(__file__).with_name("mcp-schema.py"))
mcp_schema = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mcp_schema)


class CaptureTimeoutTest(unittest.TestCase):
    def test_partial_line_never_turns_deadline_into_a_blocking_read(self):
        child = subprocess.Popen([sys.executable, "-u", "-c", "import sys,time;sys.stdout.write('{');sys.stdout.flush();time.sleep(10)"], stdout=subprocess.PIPE)
        try:
            self.assertTrue(select.select([child.stdout], [], [], 3)[0])
            pending = bytearray(os.read(child.stdout.fileno(), 1))
            self.assertEqual(pending, b"{")
            started = time.monotonic()
            with self.assertRaisesRegex(RuntimeError, "timed out"):
                mcp_schema.read_mcp_line(child.stdout, pending, started + 0.15)
            self.assertLess(time.monotonic() - started, 2)
        finally:
            child.terminate()
            child.wait(timeout=3)
            child.stdout.close()

    def test_preserves_a_following_line_in_the_buffer(self):
        pending = bytearray(b'{"id":1}\n{"id":2}\n')
        self.assertEqual(mcp_schema.read_mcp_line(None, pending, 0), b'{"id":1}\n')
        self.assertEqual(mcp_schema.read_mcp_line(None, pending, 0), b'{"id":2}\n')
        self.assertFalse(pending)

    def test_rejects_oversized_partial_response(self):
        with self.assertRaisesRegex(RuntimeError, "exceeded"):
            mcp_schema.read_mcp_line(None, bytearray(b'x' * (mcp_schema.MAX_RESPONSE_BYTES + 1)), 0)

    def test_repeated_cursor_across_empty_pages_is_rejected(self):
        cursors = iter(["a", "b", "a"])
        with self.assertRaisesRegex(RuntimeError, "repeated"):
            mcp_schema.discover_tools(lambda *_: {"tools": [], "nextCursor": next(cursors)})

    def test_page_count_bounds_unique_empty_pages(self):
        index = iter(range(1000))
        with self.assertRaisesRegex(RuntimeError, "page count"):
            mcp_schema.discover_tools(lambda *_: {"tools": [], "nextCursor": str(next(index))})

    def test_final_page_tool_count_is_checked(self):
        with self.assertRaisesRegex(RuntimeError, "tool count"):
            mcp_schema.discover_tools(lambda *_: {"tools": [{"name": str(i)} for i in range(101)]})

    def test_normal_discovery_retains_all_pages(self):
        pages = iter([{"tools": [{"name": "one"}], "nextCursor": "a"}, {"tools": [{"name": "two"}]}])
        tools = mcp_schema.discover_tools(lambda *_: next(pages))
        self.assertEqual([tool["name"] for tool in tools], ["one", "two"])

    def test_stalled_stdin_writer_respects_deadline(self):
        child = subprocess.Popen([sys.executable, "-c", "import time;time.sleep(10)"], stdin=subprocess.PIPE)
        try:
            started = time.monotonic()
            with self.assertRaisesRegex(RuntimeError, "timed out"):
                mcp_schema.write_mcp_request(child.stdin, b'x' * (4 * 1024 * 1024), started + 0.15)
            self.assertLess(time.monotonic() - started, 2)
        finally:
            child.terminate()
            child.wait(timeout=3)
            child.stdin.close()


if __name__ == "__main__":
    unittest.main()
