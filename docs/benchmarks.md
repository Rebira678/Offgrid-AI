**Hardware:** AMD Ryzen 5 PRO 5650U with Radeon Graphics, 12 threads, 14.9 GiB RAM, Zorin OS 18.1  
**Ollama:** 0.40.0, **runs per cell:** 3 (median), **date:** 2026-10-07

| Model | Prompt | Load (s) | Time to first token (s) | Generation (tok/s) | Prompt processing (tok/s) | Prompt / generated tokens |
|---|---|---|---|---|---|---|
| llama3.2:1b | short | 3.3 | 0.7 | 16.0 | 88.0 | 60 / 10 |
| llama3.2:1b | long | 3.3 | 23.7 | 13.9 | 49.7 | 1176 / 81 |
| llama3.2:3b | short | 0.0 | 1.0 | 9.9 | 61.5 | 60 / 83 |
| llama3.2:3b | long | 0.0 | 16.5 | 9.0 | 71.8 | 1176 / 96 |
