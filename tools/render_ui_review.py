"""Rasterize opt-in egui test frames. These are source renders, not OS screenshots.

Run cargo test with WB2API_UI_REVIEW_DIR set, then pass that directory here.
Only synthetic UI test data is exported; this does not access a running desktop.
"""
import json
import sys
from pathlib import Path

import numpy as np
from PIL import Image


def render(folder):
    textures = {}
    scene = None
    for path in sorted(folder.glob('frame-*.json')):
        scene = json.loads(path.read_text(encoding='utf-8'))
        for update in scene['textures']:
            w, h = update['size']
            data = np.frombuffer((folder / update['file']).read_bytes(), dtype=np.uint8).reshape(h, w, 4)
            if update['pos'] is None:
                textures[update['id']] = data.copy()
            else:
                x, y = update['pos']
                textures[update['id']][y:y+h, x:x+w] = data
    if scene is None:
        return
    w, h = int(scene['width']), int(scene['height'])
    canvas = np.zeros((h, w, 4), dtype=np.float32)
    for mesh in scene['meshes']:
        texture = textures[mesh['texture']].astype(np.float32) / 255
        th, tw = texture.shape[:2]
        vertices = np.asarray(mesh['vertices'], dtype=np.float32)
        clip = mesh['clip']
        for indices in np.asarray(mesh['indices']).reshape(-1, 3):
            v = vertices[indices]
            x0 = max(0, int(np.floor(max(v[:, 0].min(), clip[0]))))
            y0 = max(0, int(np.floor(max(v[:, 1].min(), clip[1]))))
            x1 = min(w, int(np.ceil(min(v[:, 0].max(), clip[2]))))
            y1 = min(h, int(np.ceil(min(v[:, 1].max(), clip[3]))))
            if x1 <= x0 or y1 <= y0:
                continue
            a, b, c = v[:, :2]
            denominator = (b[1]-c[1])*(a[0]-c[0]) + (c[0]-b[0])*(a[1]-c[1])
            if abs(denominator) < 1e-6:
                continue
            yy, xx = np.mgrid[y0:y1, x0:x1].astype(np.float32)
            xx += .5
            yy += .5
            alpha = ((b[1]-c[1])*(xx-c[0]) + (c[0]-b[0])*(yy-c[1])) / denominator
            beta = ((c[1]-a[1])*(xx-c[0]) + (a[0]-c[0])*(yy-c[1])) / denominator
            gamma = 1-alpha-beta
            mask = (alpha >= 0) & (beta >= 0) & (gamma >= 0)
            weights = np.stack([alpha, beta, gamma], axis=-1)
            uv = weights @ v[:, 2:4]
            tx = np.clip(uv[..., 0]*tw-.5, 0, tw-1)
            ty = np.clip(uv[..., 1]*th-.5, 0, th-1)
            ix, iy = tx.astype(int), ty.astype(int)
            fx, fy = (tx-ix)[..., None], (ty-iy)[..., None]
            jx, jy = np.minimum(ix+1, tw-1), np.minimum(iy+1, th-1)
            sample = ((1-fx)*(1-fy)*texture[iy, ix] + fx*(1-fy)*texture[iy, jx]
                      + (1-fx)*fy*texture[jy, ix] + fx*fy*texture[jy, jx])
            source = sample * ((weights @ v[:, 4:8]) / 255)
            dest = canvas[y0:y1, x0:x1]
            dest[mask] = (source + dest*(1-source[..., 3:4]))[mask]
    Image.fromarray(np.clip(canvas*255, 0, 255).astype(np.uint8)).save(folder / 'preview.png')
    print(folder / 'preview.png')


if __name__ == '__main__':
    for folder in sorted(Path(sys.argv[1]).iterdir()):
        if folder.is_dir():
            render(folder)
