"""Tests against the real model on a real GPU. Skipped unless KNOWRAG_GPU_TESTS=1.

They need the service's virtualenv (requirements.txt), a CUDA device and the ~0.5 GB of weights in
the Hugging Face cache (downloaded on the first run). CI has none of that, which is why the rest of
the suite runs on a fake model; these are the checks a fake cannot make:

  - the prefixes the service builds are the ones the model's own `query` and `document` prompts add;
  - unloading returns the VRAM, and a call after it loads the model again;
  - bf16 and fp32 agree, so allowing both is not allowing two different vector spaces;
  - the prompt reserve the Go profile keeps (128 tokens) covers the prefix with a long title.
"""

import os

import pytest

pytestmark = pytest.mark.skipif(
    os.environ.get("KNOWRAG_GPU_TESTS") != "1", reason="set KNOWRAG_GPU_TESTS=1 to run on a GPU"
)

torch = pytest.importorskip("torch")

import st_adapter  # noqa: E402  (after the skip: it must not import torch on a plain machine)
from fakes import make_settings  # noqa: E402
from model_host import ModelHost  # noqa: E402
from prompts import passage_text, query_text  # noqa: E402

PROMPT_RESERVE_TOKENS = 128  # schema.ModelProfile "embeddinggemma-2", PromptReserveTokens
MIB = 1024 * 1024


@pytest.fixture(scope="module")
def model():
    loaded = st_adapter.load_verified_model(make_settings(max_input_tokens=8192))
    yield loaded
    del loaded
    st_adapter.release_cuda_memory()


def cosine(a: list[float], b: list[float]) -> float:
    return sum(x * y for x, y in zip(a, b, strict=True))  # both are unit vectors


def test_service_prefixes_equal_the_models_own_prompts(model):
    raw = model._model
    text = "onde fica o contrato de 2026"
    ours_q = model.encode([query_text(text)], batch_size=1)[0]
    ref_q = raw.encode(text, prompt_name="query", normalize_embeddings=True).astype("float32").tolist()
    ours_d = model.encode([passage_text("", text)], batch_size=1)[0]
    ref_d = raw.encode(text, prompt_name="document", normalize_embeddings=True).astype("float32").tolist()
    assert cosine(ours_q, ref_q) > 0.999
    assert cosine(ours_d, ref_d) > 0.999


def test_the_prefix_changes_the_vector(model):
    text = "onde fica o contrato de 2026"
    bare = model.encode([text], batch_size=1)[0]
    assert cosine(bare, model.encode([query_text(text)], batch_size=1)[0]) < 0.999


def test_the_title_changes_the_vector(model):
    a = model.encode([passage_text("Contratos", "texto do corpo")], batch_size=1)[0]
    b = model.encode([passage_text("Receitas", "texto do corpo")], batch_size=1)[0]
    assert cosine(a, b) < 0.9999


def test_vectors_are_unit_length_and_768_wide(model):
    vec = model.encode(["a"], batch_size=1)[0]
    assert len(vec) == 768 and abs(cosine(vec, vec) - 1.0) < 1e-2


def test_the_prompt_reserve_covers_the_prefix_with_a_long_title(model):
    title = "Notas sobre arquitetura do sistema de busca semântica e rotinas de retenção de dados " * 4
    bare = model.count_tokens([""])[0]
    with_prompt = model.count_tokens([passage_text(title, "")])[0]
    assert len(title) > 300
    assert with_prompt - bare <= PROMPT_RESERVE_TOKENS


def test_unload_returns_the_vram_and_the_next_call_loads_again():
    settings = make_settings()
    clock = [0.0]
    host = ModelHost(lambda: st_adapter.load_verified_model(settings), st_adapter.release_cuda_memory,
                     clock=lambda: clock[0], log=lambda _line: None)
    base = torch.cuda.memory_allocated()
    with host.use() as m:
        m.encode(["warm up"], batch_size=1)
    del m  # a local that outlives the call would keep the model alive; handlers have no such local
    loaded = torch.cuda.memory_allocated() - base
    assert loaded > 300 * MIB  # the weights (~0.5 GB in bf16)

    clock[0] += 601
    assert host.unload_if_idle(600) is True
    assert torch.cuda.memory_allocated() - base < 50 * MIB, "the VRAM was not given back"

    with host.use() as m:
        assert len(m.encode(["again"], batch_size=1)[0]) == 768
    del m
    clock[0] += 601
    host.unload_if_idle(600)


def test_bf16_and_fp32_agree_so_allowing_both_is_one_vector_space():
    texts = [passage_text("Plano", "o plano de ingestão roda em horário fixo"), query_text("quando roda")]
    vectors = {}
    for precision in ("bf16", "fp32"):
        loaded = st_adapter.load_verified_model(make_settings(precision=precision))
        vectors[precision] = loaded.encode(texts, batch_size=2)
        del loaded
        st_adapter.release_cuda_memory()
    for a, b in zip(vectors["bf16"], vectors["fp32"], strict=True):
        assert cosine(a, b) > 0.98
