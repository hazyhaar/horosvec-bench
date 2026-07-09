#!/usr/bin/env python3
"""Sidecar d'embedding CPU pour hnbook-serve.

Un serveur HTTP local minimal (stdlib http.server, aucun framework web) qui charge une fois
le modèle Qwen3-Embedding-0.6b et transforme un texte de requête en un vecteur normalisé de
dimension 512. Le pipeline REPRODUIT À L'IDENTIQUE la référence de parité prouvée
(embed_queries_cpu.py) : pooling du dernier token (tokenizer padding_side=left, donc dernier
token tout court), normalisation pleine dimension, troncature Matryoshka à 512, puis
renormalisation. VARIANTE SANS EOS (aucun token de fin ajouté), conformément au choix retenu.

Protocole : POST {"text": "..."} -> {"vector": [512 flottants]}. Le texte est tronqué à
MAX_INPUT_CHARS (marge anti-abus) avant tokenisation. Le modèle est chargé au démarrage ;
toute erreur de chargement est fatale (fail-loud, aucune requête servie).

Dépendances : transformers, torch, numpy uniquement (aucun framework web).

Lancement :
    python3 embed_sidecar.py --model /inference/models/qwen3-embedding-0.6b \
        --host 127.0.0.1 --port 8471
"""
import argparse
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
import torch
import torch.nn.functional as F
from transformers import AutoModel, AutoTokenizer

DIM = 512                 # troncature Matryoshka, identique à la référence
MAX_INPUT_CHARS = 2000    # plafond de longueur d'entrée (marge anti-abus)
MAX_BODY_BYTES = 8192     # borne de taille du corps de requête accepté


class Embedder:
    """Encapsule le tokenizer et le modèle ; reproduit le pipeline de référence sans EOS."""

    def __init__(self, model_path):
        # padding_side=left : le pooling « dernier token » vaut alors la dernière position.
        self.tok = AutoTokenizer.from_pretrained(model_path, padding_side="left")
        self.model = AutoModel.from_pretrained(model_path, torch_dtype=torch.float32).eval()

    @torch.no_grad()
    def embed(self, text):
        text = text[:MAX_INPUT_CHARS]
        # Variante SANS EOS : le texte est tokenisé tel quel (aucun eos_token ajouté).
        batch = self.tok([text], padding=True, truncation=True, max_length=8000,
                         return_tensors="pt")
        out = self.model(**batch).last_hidden_state
        emb = out[:, -1, :]              # pooling dernier token
        emb = F.normalize(emb, dim=-1)   # normalisation pleine dimension (1024)
        emb = emb[:, :DIM]               # troncature Matryoshka 512
        emb = F.normalize(emb, dim=-1)   # renormalisation post-troncature
        return emb[0].numpy().astype(np.float32)


def make_handler(embedder):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def _send(self, code, payload):
            body = json.dumps(payload).encode("utf-8")
            self.send_response(code)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            if self.path == "/healthz":
                self._send(200, {"status": "ok"})
            else:
                self._send(404, {"error": "not found"})

        def do_POST(self):
            if self.path != "/embed":
                self._send(404, {"error": "not found"})
                return
            try:
                length = int(self.headers.get("Content-Length", "0"))
            except ValueError:
                self._send(400, {"error": "content-length invalide"})
                return
            if length <= 0 or length > MAX_BODY_BYTES:
                self._send(400, {"error": "corps absent ou trop volumineux"})
                return
            raw = self.rfile.read(length)
            try:
                req = json.loads(raw)
                text = req["text"]
            except (ValueError, KeyError, TypeError):
                self._send(400, {"error": "json invalide ou champ text manquant"})
                return
            if not isinstance(text, str) or not text.strip():
                self._send(400, {"error": "text vide"})
                return
            try:
                vec = embedder.embed(text)
            except Exception as exc:  # noqa: BLE001 — fail-loud, jamais un vecteur vide
                self._send(500, {"error": "echec embedding: %s" % exc})
                return
            self._send(200, {"vector": vec.tolist()})

        def log_message(self, fmt, *args):
            sys.stderr.write("embed_sidecar %s - %s\n" % (self.address_string(), fmt % args))

    return Handler


def main():
    ap = argparse.ArgumentParser(description="Sidecar d'embedding CPU pour hnbook-serve")
    ap.add_argument("--model", default="/inference/models/qwen3-embedding-0.6b")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8471)
    args = ap.parse_args()

    torch.set_num_threads(max(1, torch.get_num_threads()))
    sys.stderr.write("embed_sidecar : chargement du modèle %s ...\n" % args.model)
    embedder = Embedder(args.model)
    # Sonde de parité dimensionnelle au démarrage : un vecteur de dimension inattendue est fatal.
    probe = embedder.embed("sonde de démarrage")
    if probe.shape != (DIM,):
        sys.stderr.write("embed_sidecar FATAL : dimension %s != %d\n" % (probe.shape, DIM))
        sys.exit(2)
    sys.stderr.write("embed_sidecar : prêt sur %s:%d (dim=%d)\n" % (args.host, args.port, DIM))

    httpd = ThreadingHTTPServer((args.host, args.port), make_handler(embedder))
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        httpd.shutdown()


if __name__ == "__main__":
    main()
