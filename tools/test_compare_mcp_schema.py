import copy
import unittest

from compare_mcp_schema import compare


class CompareTests(unittest.TestCase):
    def setUp(self):
        self.before = {"tools": [{"name": "inspect", "description": "old", "inputSchema": {"type": "object"}, "annotations": {"readOnlyHint": True}}]}
        self.after = copy.deepcopy(self.before)
        self.after["tools"][0]["description"] = "new"

    def test_positive_change(self):
        self.assertEqual(compare(self.before, self.after, ["inspect"])["changed_descriptions"], ["inspect"])

    def test_same_binary_is_not_change_evidence(self):
        with self.assertRaisesRegex(ValueError, "did not change"):
            compare(self.before, self.before, ["inspect"])

    def test_schema_and_annotation_changes_fail(self):
        for field in ["inputSchema", "annotations"]:
            after = copy.deepcopy(self.after)
            after["tools"][0][field] = {}
            with self.assertRaisesRegex(ValueError, "contract changed"):
                compare(self.before, after, ["inspect"])

    def test_names_counts_and_duplicates_fail(self):
        for tools in [[], self.after["tools"] * 2, [{"name": "other"}]]:
            with self.assertRaises(ValueError):
                compare(self.before, {"tools": tools}, ["inspect"])

    def test_requires_a_positive_expectation(self):
        with self.assertRaises(ValueError):
            compare(self.before, self.after, [])


if __name__ == "__main__":
    unittest.main()
