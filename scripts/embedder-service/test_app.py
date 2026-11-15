"""The endpoint rules, against a fake model: prefixes, titles, the window, the handshake, the GPU check.

Nothing here loads torch. `Wiring` (fakes.py) is a ModelHost over a FakeTextModel with a fake clock,
so "the model was loaded by this call" and "the model was never touched" are both assertable.
"""

import pytest

from fakes import Wiring, make_settings
from settings import DENSE_DIM, MODEL_ID, PROMPT_VERSION, REVISION


def embed(w: Wiring, **req):
    return w.app.embed(req)


# --- the prefix reaches the model -------------------------------------------------------------


def test_a_query_is_encoded_with_the_search_prefix():
    w = Wiring()
    r = embed(w, inputs=["onde fica"], kind="query")
    assert r.status == 200
    assert w.model.encoded == [["task: search result | query: onde fica"]]


def test_a_passage_is_encoded_with_its_title():
    w = Wiring()
    embed(w, inputs=["corpo"], kind="passage", titles=["Plano"])
    assert w.model.encoded == [["title: Plano | text: corpo"]]


def test_a_passage_without_titles_uses_none():
    w = Wiring()
    embed(w, inputs=["corpo"], kind="passage")
    assert w.model.encoded == [["title: none | text: corpo"]]


def test_the_batch_size_setting_reaches_the_encoder():
    w = Wiring(settings=make_settings(batch_size=3))
    embed(w, inputs=["a", "b"], kind="passage")
    assert w.model.batch_sizes == [3]


def test_embed_returns_dense_and_the_prompted_token_counts_and_no_sparse_half():
    w = Wiring()
    r = embed(w, inputs=["a b"], kind="passage", titles=["T"])
    # "title: T | text: a b" -> 7 words ("title:" "T" "|" "text:" "a" "b" = 6) + 2 special tokens
    assert r.body["tokens"] == [8]
    assert len(r.body["dense"]) == 1
    assert "sparse" not in r.body


# --- refusals ---------------------------------------------------------------------------------


@pytest.mark.parametrize("req", [
    {},
    {"inputs": "hello", "kind": "query"},
    {"inputs": [], "kind": "query"},
    {"inputs": ["ok", 7], "kind": "query"},
])
def test_malformed_inputs_are_a_400_before_the_model_is_touched(req):
    w = Wiring()
    r = embed(w, **req)
    assert r.status == 400 and "non-empty array of strings" in r.body["error"]
    assert w.loads == 0


@pytest.mark.parametrize("kind", [None, "Query", "document", ""])
def test_an_unknown_or_missing_kind_is_a_400_never_a_guessed_prefix(kind):
    w = Wiring()
    r = embed(w, inputs=["a"], kind=kind)
    assert r.status == 400 and "kind" in r.body["error"]
    assert w.loads == 0


def test_titles_must_be_strings_and_match_the_inputs():
    w = Wiring()
    assert embed(w, inputs=["a"], kind="passage", titles="T").status == 400
    assert embed(w, inputs=["a"], kind="passage", titles=[1]).status == 400
    r = embed(w, inputs=["a", "b"], kind="passage", titles=["T"])
    assert r.status == 400 and "1 title" in r.body["error"]
    assert embed(w, inputs=["a"], kind="query", titles=["T"]).status == 400
    assert w.loads == 0


# --- the window (counted with the prefix and the title in front of the text) -------------------


def test_the_window_counts_the_prompted_text_and_names_the_offender():
    # The query prefix is 5 "words" in the fake tokenizer, plus 2 special tokens around everything.
    w = Wiring(settings=make_settings(max_input_tokens=12))
    ok = embed(w, inputs=["a b c d e"], kind="query")  # 5 + 5 + 2 = 12, exactly the limit
    assert ok.status == 200 and ok.body["tokens"] == [12]
    # The index matters: every input is checked, not only the first, so the caller learns which
    # chunk of a batch to split.
    r = embed(w, inputs=["a", "a b c d e f"], kind="query")  # second is 5 + 6 + 2 = 13
    assert r.status == 400
    assert (r.body["index"], r.body["tokens"], r.body["max_tokens"]) == (1, 13, 12)


def test_a_long_title_alone_can_push_a_chunk_over_the_limit():
    # The reserve on the Go side covers a normal title; this is the service refusing the abnormal one.
    w = Wiring(settings=make_settings(max_input_tokens=12))
    r = embed(w, inputs=["a b"], kind="passage", titles=["t1 t2 t3 t4 t5 t6 t7 t8"])
    assert r.status == 400 and r.body["tokens"] > 12
    assert w.model.encoded == []  # refused before encoding, not truncated


def test_one_token_under_the_limit_is_accepted_and_one_over_is_not():
    # An off-by-one refuses valid text, the opposite failure and just as silent.
    w = Wiring(settings=make_settings(max_input_tokens=11))
    assert embed(w, inputs=["a b c d"], kind="query").status == 200  # 5 + 4 + 2 = 11
    assert embed(w, inputs=["a b c d e"], kind="query").status == 400  # 12


# --- tokenize -----------------------------------------------------------------------------------


def test_tokenize_counts_the_text_alone_without_any_prefix():
    w = Wiring()
    r = w.app.tokenize({"inputs": ["a b", "c"]})
    assert (r.status, r.body) == (200, {"counts": [4, 3]})


def test_tokenize_validates_its_inputs():
    assert Wiring().app.tokenize({"inputs": []}).status == 400


# --- handshake, health, load -------------------------------------------------------------------


def test_handshake_reports_the_pinned_profile_without_loading_the_model():
    w = Wiring()
    r = w.app.handshake()
    assert r.status == 200 and w.loads == 0
    assert r.body == {
        "model_id": MODEL_ID,
        "model_revision": REVISION,
        "tokenizer_revision": REVISION,
        "dense_dim": DENSE_DIM,
        "normalized": True,
        "pooling": "mean",
        "precision": "bfloat16",
        "prompt_version": PROMPT_VERSION,
        "max_input_tokens": 64,
    }
    assert "sparse" not in r.body


def test_the_pinned_values_match_the_go_profile():
    # The Go side compares these against schema.ModelProfile "embeddinggemma-2"; a mismatch here is
    # a service every client refuses. The literals are the contract, copied from that profile.
    assert (MODEL_ID, REVISION, PROMPT_VERSION, DENSE_DIM) == (
        "google/embeddinggemma-2", "f9972ac22e696a105c482202646979e33e4b5a1e", "gemma-search-v1", 768,
    )


def test_handshake_reports_fp32_when_configured():
    w = Wiring(settings=make_settings(precision="fp32"))
    assert w.app.handshake().body["precision"] == "float32"


def test_handshake_fails_without_a_gpu_and_says_why():
    w = Wiring(cuda=lambda: False)
    r = w.app.handshake()
    assert r.status == 503 and "no CPU fallback" in r.body["error"]


def test_health_is_honest_about_loaded_and_never_loads():
    w = Wiring()
    assert w.app.health().body == {"status": "ok", "loaded": False, "cuda": True}
    assert w.loads == 0
    w.app.load()
    assert w.app.health().body["loaded"] is True


def test_health_reports_a_missing_gpu():
    assert Wiring(cuda=lambda: False).app.health().body["cuda"] is False


def test_a_health_probe_does_not_keep_the_model_alive():
    w = Wiring()
    w.app.load()
    w.clock.advance(601)
    w.app.health()
    assert w.host.unload_if_idle(600) is True


def test_load_loads_once_and_is_idempotent():
    w = Wiring()
    assert w.app.load().body == {"loaded": True}
    w.app.load()
    assert w.loads == 1
