# Builds public/raise-hand.gif (🙋 cycling through the skin tones) and the
# still public/raise-hand.png from Noto Color Emoji images (Apache 2.0):
#   for s in "" _1f3fb _1f3fc _1f3fd _1f3fe _1f3ff; do curl -sfLO "https://raw.githubusercontent.com/googlefonts/noto-emoji/main/2D/png/128/emoji_u1f64b$s.png"; done
#   python3 -m venv venv && venv/bin/pip install pillow && venv/bin/python raise-hand.py
# GIF transparency is on or off per pixel, so the soft edges are composited
# onto a light matte close to the page's light backgrounds.
from PIL import Image

tones = ["", "_1f3fb", "_1f3fc", "_1f3fd", "_1f3fe", "_1f3ff"]
size, matte = 64, (246, 244, 238)
frames = []
for t in tones:
    im = Image.open(f"emoji_u1f64b{t}.png").convert("RGBA").resize((size, size), Image.LANCZOS)
    comp = Image.alpha_composite(Image.new("RGBA", im.size, matte + (255,)), im)
    p = comp.convert("RGB").convert("P", palette=Image.ADAPTIVE, colors=255)  # index 255 stays free
    p.paste(255, im.getchannel("A").point(lambda a: 0 if a >= 96 else 255))
    p.info["transparency"] = 255
    frames.append(p)
frames[0].save("raise-hand.gif", save_all=True, append_images=frames[1:], duration=600, loop=0, disposal=2, transparency=255)
Image.open("emoji_u1f64b.png").convert("RGBA").resize((size, size), Image.LANCZOS).save("raise-hand.png", optimize=True)
