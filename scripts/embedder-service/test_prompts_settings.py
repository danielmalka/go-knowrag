"""The prefixes and the refusals: the two places a wrong value is silent downstream.

Reference strings below are copied from the pinned revision's `config_sentence_transformers.json`
("query" and "document" prompts), so a drive-by edit of `prompts.py` fails here instead of quietly
changing what every stored vector means.
"""

import pytest

from prompts import apply_prompts, passage_text, query_text
from settings import (
    EXIT_UNSUPPORTED,
    MODEL_WINDOW_TOKENS,
    UnsupportedSetting,
    load_settings,
    parse_device,
    parse_precision,
)


def test_query_prefix_is_the_card_s_retrieval_query_prompt():
    assert query_text("onde fica o contrato") == "task: search result | query: onde fica o contrato"


def test_document_prefix_carries_the_title():
    assert passage_text("Plano de 2026", "corpo") == "title: Plano de 2026 | text: corpo"


@pytest.mark.parametrize("title", ["", "   ", "\t"])
def test_a_missing_or_blank_title_becomes_none(title):
    assert passage_text(title, "corpo") == "title: none | text: corpo"


def test_passages_without_titles_use_none_for_each():
    assert apply_prompts("passage", ["a", "b"], None) == [
        "title: none | text: a",
        "title: none | text: b",
    ]


def test_titles_pair_with_their_inputs_in_order():
    out = apply_prompts("passage", ["a", "b"], ["T1", ""])
    assert out == ["title: T1 | text: a", "title: none | text: b"]


@pytest.mark.parametrize("kind", [None, "", "Query", "document", 7])
def test_an_unknown_kind_is_refused_because_the_prefix_depends_on_it(kind):
    with pytest.raises(ValueError, match="kind"):
        apply_prompts(kind, ["a"], None)


def test_titles_with_a_query_are_refused():
    with pytest.raises(ValueError, match="only valid with kind=passage"):
        apply_prompts("query", ["a"], ["T"])


def test_a_title_count_that_differs_from_the_inputs_is_refused():
    with pytest.raises(ValueError, match="1 title"):
        apply_prompts("passage", ["a", "b"], ["only one"])


# --- settings -----------------------------------------------------------------------------------


def test_defaults_are_gpu_bf16_unload_after_ten_minutes_and_a_conservative_batch():
    s = load_settings([], {})
    assert (s.device, s.precision, s.idle_unload_seconds, s.batch_size) == ("cuda:0", "bf16", 600.0, 4)
    assert s.max_input_tokens == MODEL_WINDOW_TOKENS
    assert s.host == "127.0.0.1"


def test_environment_sets_values_and_flags_win_over_it():
    env = {"KNOWRAG_EMBEDDER_IDLE_UNLOAD": "120", "KNOWRAG_EMBEDDER_BATCH_SIZE": "4",
           "KNOWRAG_EMBEDDER_PRECISION": "fp32"}
    s = load_settings(["--batch-size", "2"], env)
    assert (s.idle_unload_seconds, s.batch_size, s.precision) == (120.0, 2, "fp32")


@pytest.mark.parametrize("value", ["fp16", "float16", "FP16", "half"])
def test_fp16_is_refused_by_name(value):
    with pytest.raises(UnsupportedSetting, match="float16") as e:
        parse_precision(value)
    assert e.value.code == EXIT_UNSUPPORTED


def test_fp16_is_refused_from_the_environment_too():
    with pytest.raises(UnsupportedSetting):
        load_settings([], {"KNOWRAG_EMBEDDER_PRECISION": "fp16"})


def test_an_unknown_precision_names_the_value_and_the_offer():
    with pytest.raises(UnsupportedSetting, match="int8"):
        parse_precision("int8")


@pytest.mark.parametrize("device", ["cpu", "mps", ""])
def test_there_is_no_cpu_device(device):
    with pytest.raises(UnsupportedSetting, match="no CPU fallback"):
        parse_device(device)


@pytest.mark.parametrize("flag,value", [
    ("--batch-size", "0"), ("--batch-size", "x"), ("--idle-unload", "-1"),
    ("--max-input-tokens", "8193"), ("--max-input-tokens", "0"), ("--port", "70000"),
])
def test_out_of_range_numbers_are_refused_with_the_value(flag, value):
    with pytest.raises(UnsupportedSetting) as e:
        load_settings([flag, value], {})
    assert value in e.value.message


def test_zero_idle_unload_means_never_and_is_accepted():
    assert load_settings(["--idle-unload", "0"], {}).idle_unload_seconds == 0.0
