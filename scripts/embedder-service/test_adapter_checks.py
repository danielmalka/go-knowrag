"""`check_facts`: the load-time proof that the loaded model is what `/handshake` reports.

Pure function over a dict, so it runs without torch. The real model reaches it through
`SentenceTransformerText.facts()`, exercised by the opt-in GPU test.
"""

import pytest

from settings import DENSE_DIM
from st_adapter import check_facts
from fakes import make_settings


def good(**overrides) -> dict:
    facts = {"dense_dim": DENSE_DIM, "probe_width": DENSE_DIM, "dtype": "bfloat16",
             "pooling": "mean", "probe_norm": 1.0}
    return {**facts, **overrides}


def test_matching_facts_pass():
    check_facts(good(), make_settings())


def test_fp32_is_checked_against_the_configured_precision():
    check_facts(good(dtype="float32"), make_settings(precision="fp32"))


@pytest.mark.parametrize("field,value,needle", [
    ("dense_dim", 1024, "width"),
    ("probe_width", 512, "width"),
    ("dtype", "float16", "dtype float16"),
    ("pooling", "cls", "pooling"),
    ("probe_norm", 0.5, "not L2-normalized"),
])
def test_any_divergence_refuses_the_model_and_names_it(field, value, needle):
    with pytest.raises(RuntimeError, match=needle):
        check_facts(good(**{field: value}), make_settings())


def test_a_model_loaded_in_a_precision_other_than_configured_is_refused():
    with pytest.raises(RuntimeError, match="configured bfloat16"):
        check_facts(good(dtype="float32"), make_settings(precision="bf16"))
