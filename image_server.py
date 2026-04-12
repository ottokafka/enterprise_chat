import os
import gc
import json
import time
import base64
from io import BytesIO
from http.server import BaseHTTPRequestHandler, HTTPServer
from threading import Lock

# 1. OPTIMIZATION: Set Allocator Config to fix fragmentation
os.environ["PYTORCH_ALLOC_CONF"] = "expandable_segments:True"

import torch
from dotenv import load_dotenv
from huggingface_hub import login
from diffusers import ZImagePipeline

load_dotenv()

MODEL = os.environ.get("MODEL", "Tongyi-MAI/Z-Image-Turbo")
PORT = os.environ.get("PORT", 5002)
HF_TOKEN = os.environ.get("HF_TOKEN", "")

if HF_TOKEN:
    login(token=HF_TOKEN)

print("Loading model globally (This happens only once)...")

# 2. OPTIMIZATION: Load the model ONCE globally, directly to CPU RAM
pipe = ZImagePipeline.from_pretrained(
    MODEL,
    torch_dtype=torch.bfloat16,
    low_cpu_mem_usage=True, 
)

# 3. OPTIMIZATION: SEQUENTIAL CPU OFFLOAD (The Magic Fix)
# This keeps 99% of the model in System RAM. It only moves the specific 
# layer it is currently calculating to the 3.6GB of VRAM you have left.
pipe.enable_sequential_cpu_offload()

# VAE optimizations to save memory during the final image decoding step
if hasattr(pipe.vae, "enable_slicing"):
    pipe.vae.enable_slicing()
if hasattr(pipe.vae, "enable_tiling"):
    pipe.vae.enable_tiling()

# Global lock to prevent simultaneous requests crashing the VRAM
generation_lock = Lock()

def generate_image(prompt, height, width, quality):
    with generation_lock:
        print("--- Starting Generation Sequence ---")
        steps = int(quality) if quality and int(quality) > 0 else 9

        try:
            image = pipe(
                prompt=prompt,
                height=height,
                width=width,
                num_inference_steps=steps, 
                guidance_scale=0.0,         
                generator=torch.Generator("cuda").manual_seed(42),
            ).images[0]

            buffered = BytesIO()
            image.save(buffered, format="JPEG", quality=85)
            img_str = base64.b64encode(buffered.getvalue()).decode("utf-8")
            return json.dumps({"created": int(time.time()), "data": [{"b64_json": img_str}]})

        except Exception as e:
            print(f"Generation Error: {e}")
            raise e
        finally:
            # Clear CUDA cache after generation to keep your 3.6GB free
            torch.cuda.empty_cache()
            torch.cuda.ipc_collect()

class SimpleHTTPRequestHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path == "/v1/images/generations":
            try:
                content_length = int(self.headers["Content-Length"])
                post_data = self.rfile.read(content_length).decode("utf-8")
                data = json.loads(post_data)
                
                user_prompt = data.get("prompt", "Young Chinese woman at the beach")
                size = data.get("size", "1024x1024")
                quality = data.get("quality", 9)
                try:
                    height, width = map(int, size.split("x"))
                except ValueError:
                    height, width = 1024, 1024

                print(f"Request: {user_prompt} | Size: {width}x{height} | Steps: {quality}")
                
                img_str = generate_image(user_prompt, height, width, quality)

                self.send_response(200)
                self.send_header("Content-type", "application/json")
                self.end_headers()
                self.wfile.write(img_str.encode("utf-8"))
            
            except Exception as e:
                print(f"Server Error: {e}")
                self.send_response(500)
                self.send_header("Content-type", "text/plain")
                self.end_headers()
                self.wfile.write(str(e).encode("utf-8"))
        else:
            self.send_response(404)
            self.send_header("Content-type", "text/plain")
            self.end_headers()
            self.wfile.write(b"404 Not Found")

def run(server_class=HTTPServer, handler_class=SimpleHTTPRequestHandler, port=int(PORT)):
    server_address = ("", port)
    httpd = server_class(server_address, handler_class)
    print(f"Starting httpd server on port {port} using model {MODEL}...")
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass
    httpd.server_close()
    print("Server stopped.")

if __name__ == "__main__":
    run()