"""The task prefixes EmbeddingGemma 2 was trained with, applied here and nowhere else.

The model card's retrieval prompts, as `config_sentence_transformers.json` ships them at the pinned
revision:

    query     "task: search result | query: " + text
    document  "title: " + (title or "none") + " | text: " + text

A prefix that is missing, or applied to the wrong side, does not raise: the vectors still have the
right shape and the search merely gets worse (plan risk R4). That is why the service owns the
prefixes instead of the Go client, why `/embed` refuses a `kind` it does not know, and why the
scheme has a version (`settings.PROMPT_VERSION`) that rides in the handshake and in `point_hash`.
Changing any string below without bumping that version makes old points and new queries disagree
without a single error.
"""

from collections.abc import Sequence

QUERY_PREFIX = "task: search result | query: "
NO_TITLE = "none"


def query_text(text: str) -> str:
    return QUERY_PREFIX + text


def passage_text(title: str, text: str) -> str:
    return f"title: {title.strip() or NO_TITLE} | text: {text}"


def apply_prompts(kind: str, texts: Sequence[str], titles: Sequence[str] | None) -> list[str]:
    """Texts exactly as the model must read them for this request.

    `titles` belongs to documents: a query has no title in the scheme, so a request that sends
    both is a caller mixing the two sides, and it is refused rather than guessed at.
    """
    if kind == "query":
        if titles is not None:
            raise ValueError("`titles` is only valid with kind=passage; a query has no title")
        return [query_text(t) for t in texts]
    if kind == "passage":
        if titles is not None and len(titles) != len(texts):
            raise ValueError(f"{len(titles)} title(s) for {len(texts)} input(s); one title per input")
        names = titles if titles is not None else [""] * len(texts)
        return [passage_text(title, text) for title, text in zip(names, texts, strict=True)]
    raise ValueError(f'`kind` must be "query" or "passage", got {kind!r}')
