import {test, expect} from '@playwright/test';
import {openPlayer} from './player-fixture.mjs';

const files=[
  {index:0,name:'01.mp4',size:512},
  {index:4,name:'02.mp4',size:512},
  {index:19,name:'S03E05 — 1080p (2026).mkv',size:512},
];
for(const [language,count,label] of [
  ['ru','3 видео','Видео:'],['en','3 videos','Video:'],
  ['zh-Hans','3 个视频','视频：'],['zh-Hant','3 部影片','影片：'],
]){
 test(`${language}: card and player choose original filenames with ordinal counters`,async({page})=>{
  const row=await openPlayer(page,{language,files});
  const starts=[];
  page.on('request',r=>{if(r.method()==='POST'&&r.url().includes('/api/hls/prepare'))starts.push(new URL(r.url()).searchParams.get('file'))});
  await expect(row.locator('.download-videos')).toHaveText(count);
  await row.locator('.download-videos').click();
  await expect(page.locator('#playerWorkspace')).toBeHidden();
  expect(starts).toEqual([]);
  const menu=page.locator('#videoMenu');
  for(const file of files)await expect(menu.getByRole('menuitemradio',{name:file.name,exact:true})).toBeVisible();
  await expect(menu.getByRole('menuitemradio',{name:'02.mp4',exact:true})).toHaveAttribute('aria-checked','true');
  await menu.getByRole('menuitemradio',{name:'01.mp4',exact:true}).click();
  await expect.poll(()=>page.locator('#video').evaluate(v=>v.duration)).toBe(120);
  expect(starts).toEqual(['0']);
  await expect(page.locator('#selectedFile')).toHaveText('01.mp4');
  const separator=language.startsWith('zh')?'':' ';
  await expect(page.locator('#videoMenuLabel')).toHaveText(`${label}${separator}1/3`);
  await page.locator('#videoMenuButton').click();
  await expect(menu.getByRole('menuitemradio',{name:'01.mp4',exact:true})).toHaveAttribute('aria-checked','true');
  await menu.getByRole('menuitemradio',{name:files[2].name,exact:true}).click();
  await expect(page.locator('#videoMenuLabel')).toHaveText(`${label}${separator}3/3`);
  await expect(page.locator('#selectedFile')).toHaveText(files[2].name);
  await expect.poll(()=>page.locator('#video').evaluate(v=>v.duration)).toBe(120);
  expect(await page.locator('#video').evaluate(v=>v.currentTime)).toBe(0);
  expect(starts).toEqual(['0','19']);
 });
}

test('single-file cards and players omit the file switcher',async({page})=>{
 const row=await openPlayer(page,{files:[files[1]]});
 await expect(row.locator('.download-videos')).toBeHidden();
 await row.locator('.download-open').click();
 await expect.poll(()=>page.locator('#video').evaluate(v=>v.duration)).toBe(120);
 await expect(page.locator('#videoMenuButton')).toBeHidden();
 await expect(page.locator('#previousVideo')).toBeHidden();
 await expect(page.locator('#nextVideo')).toBeHidden();
 await expect(page.locator('#selectedFile')).toHaveText('02.mp4');
});

test('opening another card list preserves playback; choosing the current file is a no-op',async({page})=>{
 const row=await openPlayer(page,{files});
 await row.locator('.download-open').click();
 await expect.poll(()=>page.locator('#video').evaluate(v=>v.duration)).toBe(120);
 await page.locator('#video').evaluate(v=>{v.currentTime=42});
 await expect.poll(()=>page.locator('#video').evaluate(v=>v.currentTime)).toBe(42);
 const requests=[];
 page.on('request',r=>{if(r.method()==='POST'&&r.url().includes('/api/hls/prepare'))requests.push(r.url())});
 await page.locator('.download-row[data-id="other"] .download-videos').click();
 await expect(page.locator('#selectedFile')).toHaveText('02.mp4');
 expect(await page.locator('#video').evaluate(v=>v.currentTime)).toBe(42);
 await page.keyboard.press('Escape');
 await expect(page.locator('.download-row[data-id="other"] .download-videos')).toBeFocused();
 await page.locator('#videoMenuButton').click();
 await page.getByRole('menuitemradio',{name:'02.mp4',exact:true}).click();
 expect(await page.locator('#video').evaluate(v=>v.currentTime)).toBe(42);
 expect(requests).toEqual([]);
});

for(const width of [390,1280]){
 test(`${width}: long original filenames fit and keyboard selection survives refresh`,async({page})=>{
  await page.setViewportSize({width,height:844});
  const names=Array.from({length:40},(_,i)=>({index:i*2,name:`${String(i+1).padStart(2,'0')} — ${'Длинное_имя_'.repeat(12)}.mp4`,size:512}));
  names[0].name='<img src=x onerror=alert(1)>.mp4';
  const row=await openPlayer(page,{files:names});
  const toggle=row.locator('.download-videos');
  for(const size of [16,32]){
   await page.evaluate(size=>document.documentElement.style.fontSize=`${size}px`,size);
   await toggle.press('Enter');
   const menu=page.locator('#videoMenu');
   await expect(menu).toBeVisible();
   const box=await menu.boundingBox();
   expect(box.x).toBeGreaterThanOrEqual(0);expect(box.y).toBeGreaterThanOrEqual(0);
   expect(box.x+box.width).toBeLessThanOrEqual(width);expect(box.y+box.height).toBeLessThanOrEqual(844);
   expect(await menu.evaluate(el=>el.scrollWidth<=el.clientWidth+1)).toBe(true);
   await expect(menu.locator('img')).toHaveCount(0);
   await page.keyboard.press('End');
   const last=menu.getByRole('menuitemradio',{name:names.at(-1).name,exact:true});
   await expect(last).toBeFocused();await expect(last).toBeInViewport();
   await page.evaluate(()=>window.dispatchEvent(new Event('focus')));
   await expect(last).toBeFocused();
   await expect(page.locator('#playerWorkspace')).toBeHidden();
   await page.keyboard.press('Home');
   await expect(menu.getByRole('menuitemradio',{name:names[0].name,exact:true})).toBeFocused();
   await page.keyboard.press('Escape');
   await expect(toggle).toBeFocused();
  }
  await toggle.click();
  await page.keyboard.press('Tab');
  await expect(page.locator('#videoMenu')).toBeHidden();
  await expect(row.locator('.download-storage-button')).toBeFocused();
  await page.keyboard.press('Tab');
  await expect(row.locator('.download-more')).toBeFocused();
 });
}

test('file, sound and action menus are mutually exclusive and close outside',async({page})=>{
 const row=await openPlayer(page,{files});
 await row.locator('.download-open').click();
 await expect.poll(()=>page.locator('#video').evaluate(v=>v.duration)).toBe(120);
 await page.locator('#audioMenuButton').click();
 await page.locator('#videoMenuButton').click();
 await expect(page.locator('#audioMenu')).toBeHidden();
 await expect(page.locator('#videoMenu')).toBeVisible();
 await row.locator('.download-more').click();
 await expect(page.locator('#videoMenu')).toBeHidden();
 await row.locator('.download-videos').click();
 await expect(row.locator('.download-options')).toBeHidden();
 await page.locator('.brand').click();
 await expect(page.locator('#videoMenu')).toBeHidden();
 await expect(row.locator('.download-videos')).toBeFocused();
});

test.describe('touch file picker',()=>{
 test.use({hasTouch:true,isMobile:true,viewport:{width:390,height:844}});
 for(const language of ['en','ru','zh-Hans','zh-Hant']){
  test(`${language}: wide and enlarged text keeps close beside the title and controls inside the viewport`,async({page})=>{
   const row=await openPlayer(page,{files,language});
   // Reproduce wider Linux font metrics even when the host defaults to Noto Sans.
   await page.addStyleTag({content:':root { --font-main: "DejaVu Sans", monospace; }'});
   await row.locator('.download-open').tap();
   await expect.poll(()=>page.locator('#video').evaluate(v=>v.duration)).toBe(120);
   for(const width of [390,320]){
    await page.setViewportSize({width,height:844});
    for(const fontSize of [16,32]){
     await page.evaluate(size=>document.documentElement.style.fontSize=`${size}px`,fontSize);
     const controls=page.locator('#previousVideo, #videoMenuButton, #nextVideo, #audioMenuButton, #subtitlesMenuButton, #closePlayer');
     const boxes=await controls.evaluateAll(elements=>elements.map(el=>{
      const {x,y,width,height}=el.getBoundingClientRect();
      return {x,y,width,height,overflow:el.scrollWidth>el.clientWidth+1};
     }));
     const close=boxes.at(-1);
     const title=await page.locator('.player-identity').boundingBox();
     expect(Math.abs(title.y-close.y),`${width}px / ${fontSize}px`).toBeLessThan(1);
     expect(title.x+title.width).toBeLessThanOrEqual(close.x);
     expect(boxes[0].y).toBeGreaterThanOrEqual(title.y+title.height);
     for(const box of boxes){
      expect(box.x).toBeGreaterThanOrEqual(0);
      expect(box.x+box.width).toBeLessThanOrEqual(width);
      expect(box.height).toBeGreaterThanOrEqual(44);
      expect(box.overflow).toBe(false);
     }

     expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
    }
   }
   await page.locator('#closePlayer').tap();
   await expect(page.locator('#playerWorkspace')).toBeHidden();
  });
 }
 test('the count opens a list and tapping a filename starts that file',async({page})=>{
  const row=await openPlayer(page,{files,language:'ru'});
  await row.locator('.download-videos').tap();
  await expect(page.locator('#playerWorkspace')).toBeHidden();
  await page.getByRole('menuitemradio',{name:'01.mp4',exact:true}).tap();
  await expect.poll(()=>page.locator('#video').evaluate(v=>v.duration)).toBe(120);
  await expect(page.locator('#videoMenuLabel')).toHaveText('Видео: 1/3');
  const selector=await page.locator('#videoMenuButton').boundingBox();
  const close=await page.locator('#closePlayer').boundingBox();
  const title=await page.locator('.player-identity').boundingBox();
  expect(Math.abs(title.y-close.y)).toBeLessThan(1);
  expect(selector.y).toBeGreaterThanOrEqual(title.y+title.height);
 });
});
