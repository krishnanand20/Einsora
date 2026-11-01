"""Generate a tiny, random safetensors model for the opt-in native endpoint integration test.

Run with a Python environment containing the optional serve extra. No model is downloaded.
"""

import json
import sys
from pathlib import Path


def main() -> None:
    import mlx.core as mx
    from mlx_lm.models.llama import Model, ModelArgs
    from tokenizers import Tokenizer
    from tokenizers.models import WordLevel
    from tokenizers.pre_tokenizers import Whitespace
    from transformers import PreTrainedTokenizerFast

    dest = Path(sys.argv[1])
    dest.mkdir(parents=True, exist_ok=True)
    config = {
        "model_type": "llama",
        "hidden_size": 16,
        "num_hidden_layers": 1,
        "intermediate_size": 32,
        "num_attention_heads": 2,
        "num_key_value_heads": 2,
        "rms_norm_eps": 1e-5,
        "vocab_size": 8,
        "max_position_embeddings": 8192,
        "eos_token_id": 2,
        "bos_token_id": 1,
    }
    mx.random.seed(7)
    model = Model(ModelArgs.from_dict(config))
    model.save_weights(str(dest / "model.safetensors"))
    (dest / "config.json").write_text(json.dumps(config), encoding="utf-8")
    vocabulary = {
        word: i
        for i, word in enumerate(
            ["<unk>", "<bos>", "<eos>", "hello", "world", "user", "assistant", "system"]
        )
    }
    unknown, beginning, end = "<unk>", "<bos>", "<eos>"
    tokenizer = Tokenizer(WordLevel(vocabulary, unk_token=unknown))
    tokenizer.pre_tokenizer = Whitespace()
    wrapped = PreTrainedTokenizerFast(
        tokenizer_object=tokenizer, unk_token=unknown, bos_token=beginning, eos_token=end
    )
    wrapped.chat_template = (
        "{% for m in messages %}{{ m['role'] }} {{ m['content'] }} {% endfor %}"
        "{% if add_generation_prompt %}assistant {% endif %}"
    )
    wrapped.save_pretrained(dest)


if __name__ == "__main__":
    main()
