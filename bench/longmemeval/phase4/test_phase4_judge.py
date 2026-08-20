"""Tests for cmd_judge abstention prompt routing and _report."""

import json
import os
import sys
import tempfile
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))

sys.path.insert(0, os.path.dirname(__file__))


def _make_args(dataset, hyp, out_path, model="test-model", provider="openai"):
    return mock.MagicMock(
        dataset=dataset, hyp=hyp, out_path=out_path,
        judged=out_path, model=model, provider=provider,
        longmemeval_src="/fake/src", api_base_url=None,
    )


def _make_dataset(*qids):
    return [{"question_id": q, "question_type": "fact",
             "question": f"q-{q}", "answer": f"a-{q}"} for q in qids]


def _make_hyps(*qids):
    return [{"question_id": q, "hypothesis": f"h-{q}"} for q in qids]


def _write_jsonl(path, rows):
    with open(path, "w") as f:
        for r in rows:
            f.write(json.dumps(r) + "\n")


def _read_jsonl(path):
    return [json.loads(l) for l in open(path) if l.strip()]


# ---------------------------------------------------------------------------
# mock stubs
# ---------------------------------------------------------------------------
_fake_anscheck_prompt = mock.MagicMock(return_value="ANSWER_CHECK_PROMPT")
_fake_abstention_prompt = mock.MagicMock(return_value="ABSTENTION_PROMPT")


def _run_judge(dataset, hyps, out_path, resp_text="yes"):
    """Run cmd_judge with mocked dependencies."""
    _fake_abstention_prompt.reset_mock()
    _fake_anscheck_prompt.reset_mock()
    with mock.patch.dict(sys.modules, {
        "run_generation": mock.MagicMock(),
        "evaluate_qa": mock.MagicMock(get_anscheck_prompt=_fake_anscheck_prompt),
        "abstention_prompt": mock.MagicMock(get_abstention_prompt=_fake_abstention_prompt),
        "tiktoken": mock.MagicMock(),
    }):
        with mock.patch("phase4_run.import_official",
                        return_value=(None, _fake_anscheck_prompt)):
            with mock.patch("phase4_run.get_key", return_value="fake-key"):
                with mock.patch("phase4_run.chat",
                                return_value=resp_text):
                    with mock.patch("phase4_run._report"):
                        with mock.patch("phase4_run.load_done", return_value=set()):
                            from phase4_run import cmd_judge
                            args = _make_args(dataset, hyps, out_path)
                            cmd_judge(args)


def test_abs_uses_abstention_prompt():
    """Questions ending in _abs should use get_abstention_prompt."""
    with tempfile.TemporaryDirectory() as td:
        ds_path = os.path.join(td, "dataset.json")
        hyp_path = os.path.join(td, "hyp.jsonl")
        out_path = os.path.join(td, "out.jsonl")

        dataset = _make_dataset("q1_abs", "q2", "q3_abs")
        hyps = _make_hyps("q1_abs", "q2", "q3_abs")
        _write_jsonl(hyp_path, hyps)

        with open(ds_path, "w") as f:
            json.dump(dataset, f)

        _run_judge(ds_path, hyp_path, out_path)

        # abstention_prompt was called for _abs questions
        assert _fake_abstention_prompt.call_count == 2
        _fake_abstention_prompt.assert_any_call("q-q1_abs", "h-q1_abs")
        _fake_abstention_prompt.assert_any_call("q-q3_abs", "h-q3_abs")


def test_non_abs_uses_anscheck_prompt():
    """Questions NOT ending in _abs should use get_anscheck_prompt."""
    with tempfile.TemporaryDirectory() as td:
        ds_path = os.path.join(td, "dataset.json")
        hyp_path = os.path.join(td, "hyp.jsonl")
        out_path = os.path.join(td, "out.jsonl")

        dataset = _make_dataset("q1_abs", "q2", "q3_abs")
        hyps = _make_hyps("q1_abs", "q2", "q3_abs")
        _write_jsonl(hyp_path, hyps)

        with open(ds_path, "w") as f:
            json.dump(dataset, f)

        _run_judge(ds_path, hyp_path, out_path)

        # anscheck was called once for the non-abs question
        assert _fake_anscheck_prompt.call_count == 1
        _fake_anscheck_prompt.assert_called_once_with(
            "fact", "q-q2", "a-q2", "h-q2", abstention=False)


def test_output_contains_abstention_field():
    """Output rows should include the abstention boolean field."""
    with tempfile.TemporaryDirectory() as td:
        ds_path = os.path.join(td, "dataset.json")
        hyp_path = os.path.join(td, "hyp.jsonl")
        out_path = os.path.join(td, "out.jsonl")

        dataset = _make_dataset("q1_abs", "q2")
        hyps = _make_hyps("q1_abs", "q2")
        _write_jsonl(hyp_path, hyps)

        with open(ds_path, "w") as f:
            json.dump(dataset, f)

        _run_judge(ds_path, hyp_path, out_path)

        rows = _read_jsonl(out_path)
        by_id = {r["question_id"]: r for r in rows}
        assert by_id["q1_abs"]["abstention"] is True
        assert by_id["q2"]["abstention"] is False


def test_label_from_yes_response():
    """'yes' in response should yield autoeval_label True."""
    with tempfile.TemporaryDirectory() as td:
        ds_path = os.path.join(td, "dataset.json")
        hyp_path = os.path.join(td, "hyp.jsonl")
        out_path = os.path.join(td, "out.jsonl")

        dataset = _make_dataset("q1")
        hyps = _make_hyps("q1")
        _write_jsonl(hyp_path, hyps)

        with open(ds_path, "w") as f:
            json.dump(dataset, f)

        _run_judge(ds_path, hyp_path, out_path, resp_text="Yes, the model abstained correctly.")

        rows = _read_jsonl(out_path)
        assert rows[0]["autoeval_label"] is True


def test_label_from_no_response():
    """'no' in response should yield autoeval_label False."""
    with tempfile.TemporaryDirectory() as td:
        ds_path = os.path.join(td, "dataset.json")
        hyp_path = os.path.join(td, "hyp.jsonl")
        out_path = os.path.join(td, "out.jsonl")

        dataset = _make_dataset("q1")
        hyps = _make_hyps("q1")
        _write_jsonl(hyp_path, hyps)

        with open(ds_path, "w") as f:
            json.dump(dataset, f)

        _run_judge(ds_path, hyp_path, out_path, resp_text="No, it hallucinated.")

        rows = _read_jsonl(out_path)
        assert rows[0]["autoeval_label"] is False


def test_mixed_dataset_correct_routing():
    """Full mix: 3 _abs and 2 regular, verify counts."""
    _fake_abstention_prompt.reset_mock()
    _fake_anscheck_prompt.reset_mock()

    with tempfile.TemporaryDirectory() as td:
        ds_path = os.path.join(td, "dataset.json")
        hyp_path = os.path.join(td, "hyp.jsonl")
        out_path = os.path.join(td, "out.jsonl")

        dataset = _make_dataset("a1_abs", "b1", "a2_abs", "b2", "a3_abs")
        hyps = _make_hyps("a1_abs", "b1", "a2_abs", "b2", "a3_abs")
        _write_jsonl(hyp_path, hyps)

        with open(ds_path, "w") as f:
            json.dump(dataset, f)

        _run_judge(ds_path, hyp_path, out_path)

        rows = _read_jsonl(out_path)
        abs_rows = [r for r in rows if r["abstention"]]
        non_abs_rows = [r for r in rows if not r["abstention"]]
        assert len(abs_rows) == 3
        assert len(non_abs_rows) == 2
        assert _fake_abstention_prompt.call_count == 3
        assert _fake_anscheck_prompt.call_count == 2


# ---------------------------------------------------------------------------
# _report blended accuracy tests
# ---------------------------------------------------------------------------

def _write_report_rows(path, rows):
    with open(path, "w") as f:
        for r in rows:
            f.write(json.dumps(r) + "\n")


def test_report_shows_three_accuracies(capsys):
    """_report should print blended, non-abstention, and abstention accuracies."""
    with mock.patch.dict(sys.modules, {
        "run_generation": mock.MagicMock(),
        "evaluate_qa": mock.MagicMock(),
        "abstention_prompt": mock.MagicMock(),
        "tiktoken": mock.MagicMock(),
    }):
        from phase4_run import _report

        with tempfile.TemporaryDirectory() as td:
            path = os.path.join(td, "results.jsonl")
            rows = [
                {"question_type": "fact", "abstention": False, "autoeval_label": True},
                {"question_type": "fact", "abstention": False, "autoeval_label": False},
                {"question_type": "fact", "abstention": True,  "autoeval_label": True},
            ]
            _write_report_rows(path, rows)
            _report(path)
            out = capsys.readouterr().out

            assert "blended 500-question" in out
            assert "non-abstention 470-question" in out
            assert "abstention 30-question" in out

            # blended: 2/3, non-abstention: 1/2, abstention: 1/1
            assert "0.6667" in out
            assert "0.5000" in out
            assert "1.0000" in out


def test_report_per_type_non_abstention_only(capsys):
    """Per-type breakdown should only include non-abstention rows."""
    with mock.patch.dict(sys.modules, {
        "run_generation": mock.MagicMock(),
        "evaluate_qa": mock.MagicMock(),
        "abstention_prompt": mock.MagicMock(),
        "tiktoken": mock.MagicMock(),
    }):
        from phase4_run import _report

        with tempfile.TemporaryDirectory() as td:
            path = os.path.join(td, "results.jsonl")
            rows = [
                {"question_type": "fact", "abstention": False, "autoeval_label": True},
                {"question_type": "fact", "abstention": True,  "autoeval_label": False},
                {"question_type": "temporal", "abstention": False, "autoeval_label": False},
            ]
            _write_report_rows(path, rows)
            _report(path)
            out = capsys.readouterr().out

            assert "Per question_type (non-abstention):" in out
            assert "fact" in out
            assert "temporal" in out
            # fact non-abstention count should be 1 (the True one), not 2
            assert "n=1" in out


def test_report_empty_abstention(capsys):
    """All rows non-abstention: abstention acc should show 0.0000 with n=0."""
    with mock.patch.dict(sys.modules, {
        "run_generation": mock.MagicMock(),
        "evaluate_qa": mock.MagicMock(),
        "abstention_prompt": mock.MagicMock(),
        "tiktoken": mock.MagicMock(),
    }):
        from phase4_run import _report

        with tempfile.TemporaryDirectory() as td:
            path = os.path.join(td, "results.jsonl")
            rows = [
                {"question_type": "fact", "abstention": False, "autoeval_label": True},
                {"question_type": "fact", "abstention": False, "autoeval_label": True},
            ]
            _write_report_rows(path, rows)
            _report(path)
            out = capsys.readouterr().out

            assert "abstention 30-question" in out
            assert "  Abstention:     0" in out


def test_report_empty_file():
    """Empty file should sys.exit."""
    with mock.patch.dict(sys.modules, {
        "run_generation": mock.MagicMock(),
        "evaluate_qa": mock.MagicMock(),
        "abstention_prompt": mock.MagicMock(),
        "tiktoken": mock.MagicMock(),
    }):
        from phase4_run import _report

        with tempfile.TemporaryDirectory() as td:
            path = os.path.join(td, "empty.jsonl")
            open(path, "w").close()
            try:
                _report(path)
                assert False, "Expected SystemExit"
            except SystemExit as e:
                assert "no rows" in str(e)
