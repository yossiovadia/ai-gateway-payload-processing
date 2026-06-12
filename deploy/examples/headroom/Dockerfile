FROM python:3.11-slim

RUN pip install --no-cache-dir "headroom-ai[proxy]==0.25.0" onnxruntime

# Writable dirs for OpenShift random UID
ENV HF_HOME=/opt/huggingface
ENV HOME=/opt/app
ENV HEADROOM_DATA_DIR=/opt/headroom-data
RUN mkdir -p /opt/huggingface /opt/app /opt/headroom-data

# Pre-download Kompress ONNX model + ModernBERT tokenizer
RUN python -c "\
from huggingface_hub import snapshot_download; \
snapshot_download('chopratejas/kompress-v2-base'); \
snapshot_download('answerdotai/ModernBERT-base')"

RUN chmod -R 777 /opt/huggingface /opt/app /opt/headroom-data

COPY compress-raw-server.py /opt/app/compress-raw-server.py

EXPOSE 8787 8788
ENTRYPOINT ["python3", "/opt/app/compress-raw-server.py"]
