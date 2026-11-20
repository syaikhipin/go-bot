#!/usr/bin/env python3
"""Render a reviewed screenshot recording into a focused, captioned MP4.

Input recipe is private (raw console captures must stay out of Git). Each scene
specifies a screenshot, crop, redactions, focus rectangle, captions and duration.
Only reviewed output media is copied into a plugin's public assets directory.
Requires Pillow, ffmpeg and a font supporting the requested caption language.
"""
import argparse
import json
import subprocess
import tempfile
from pathlib import Path
from PIL import Image, ImageDraw, ImageFont


def wrap(draw, text, font, width):
    lines=[]
    for paragraph in text.split('\n'):
        line=''
        for char in paragraph:
            if draw.textlength(line+char,font=font)>width and line:
                lines.append(line.strip());line=''
            line+=char
        lines.append(line.strip())
    return lines


def render(recipe, language, output, font_path):
    with tempfile.TemporaryDirectory(prefix='go-bot-tutorial-') as temp:
        work=Path(temp);clips=[]
        for index,scene in enumerate(recipe['scenes']):
            picture=Image.open(scene['image']).convert('RGB')
            draw=ImageDraw.Draw(picture)
            for box in scene.get('redactions',[]):
                draw.rounded_rectangle(tuple(box),radius=6,fill='#e4e7ec')
            left,top,right,bottom=scene['crop']
            picture=picture.crop((left,top,right,bottom))
            width,height=picture.size
            padded_height=round(width*9/16)
            pad_y=(padded_height-height)//2
            canvas=Image.new('RGB',(width,padded_height),'#f4f5f8')
            canvas.paste(picture,(0,pad_y));draw=ImageDraw.Draw(canvas)
            x1,y1,x2,y2=scene['focus']
            box=(x1-left,y1-top+pad_y,x2-left,y2-top+pad_y)
            draw.rounded_rectangle(box,radius=10,outline='#7466c4',width=5)
            image=work/f'{index}.png';canvas.save(image)
            caption=Image.new('RGBA',(1920,180),(20,28,43,238));draw=ImageDraw.Draw(caption)
            title_font=ImageFont.truetype(font_path,35)
            body_font=ImageFont.truetype(font_path,27)
            title,body=scene['captions'][language]
            draw.text((48,24),title,font=title_font,fill='white')
            for line_index,line in enumerate(wrap(draw,body,body_font,1810)[:2]):
                draw.text((48,79+line_index*36),line,font=body_font,fill='#e1e5ee')
            overlay=work/f'caption-{index}.png';caption.save(overlay)
            frames=round(scene.get('seconds',6)*30)
            focus_x=(box[0]+box[2])/2;focus_y=(box[1]+box[3])/2
            amount=scene.get('zoom',1.5)-1
            # Establish full-page context, zoom to the control, hold, then pull
            # back before the next real recorded page. No synthetic UI frames.
            rise=f'min(max((on-24)/45,0),1)'
            fall=f'min(max((on-{frames-42})/30,0),1)'
            zoom=f'1+{amount}*({rise}-{fall})'
            x=f'max(0,min(iw-iw/zoom,{focus_x}-iw/(2*zoom)))'
            y=f'max(0,min(ih-ih/zoom,{focus_y}-ih/(2*zoom)))'
            video=work/f'{index}.mp4'
            graph=f"[0:v]zoompan=z='{zoom}':x='{x}':y='{y}':d={frames}:s=1920x1080:fps=30[shot];[shot][1:v]overlay=0:0,format=yuv420p[v]"
            subprocess.run(['ffmpeg','-hide_banner','-loglevel','error','-y','-i',str(image),'-i',str(overlay),'-filter_complex',graph,'-map','[v]','-frames:v',str(frames),'-c:v','libx264','-preset','fast','-crf','23','-pix_fmt','yuv420p',str(video)],check=True)
            clips.append(video)
        listing=work/'clips.txt';listing.write_text(''.join("file '"+str(clip)+"'\n" for clip in clips))
        output.parent.mkdir(parents=True,exist_ok=True)
        subprocess.run(['ffmpeg','-hide_banner','-loglevel','error','-y','-f','concat','-safe','0','-i',str(listing),'-c','copy','-movflags','+faststart',str(output)],check=True)
        subprocess.run(['ffmpeg','-hide_banner','-loglevel','error','-y','-ss','1','-i',str(output),'-frames:v','1',str(output.with_suffix('.png'))],check=True)


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('recipe',type=Path)
    parser.add_argument('--language',choices=['en','zh-CN'],required=True)
    parser.add_argument('--font',type=Path,required=True)
    parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args()
    render(json.loads(args.recipe.read_text()),args.language,args.output,str(args.font))
