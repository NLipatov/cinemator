import { test, expect } from '@playwright/test';
import { openPlayer } from './player-fixture.mjs';

const preview = `<svg xmlns="http://www.w3.org/2000/svg" width="3200" height="360">${['#e33','#3d3','#35e','#ed3','#e3d'].map((fill,i)=>`<rect x="${i*640}" width="640" height="360" fill="${fill}"/>`).join('')}</svg>`;

for (const touch of [false, true]) {
  test.describe(touch ? 'touch cards' : 'keyboard cards', () => {
    test.use({ hasTouch: touch, viewport: touch ? {width:390,height:844} : {width:1280,height:900} });
    test('preview starts immediately and a saved file resumes after reload', async ({ page }) => {
      const requests=[];
      page.on('request', request=>{if (request.url().includes('/api/hls/prepare')) requests.push(request.url());});
      const row=await openPlayer(page);
      await expect(row.locator('select')).toHaveCount(0);
      expect(requests).toEqual([]);
      if(touch) await row.locator('.download-thumbnail').tap();
      else await row.locator('.download-open').press('Enter');
      await expect.poll(()=>page.locator('#video').evaluate(video=>video.duration)).toBe(120);
      await page.locator('#video').evaluate(async video=>{
        const done=new Promise(resolve=>video.addEventListener('seeked',resolve,{once:true}));
        video.currentTime=36;await done;
      });
      await page.locator('#closePlayer').click();
      await expect(row.locator('.download-resume')).toHaveText('Continue from 0:36');
      await expect(row.locator('.download-duration')).toHaveText('2:00');
      expect(await row.locator('.download-watched').evaluate(el=>getComputedStyle(el,'::before').width)).not.toBe('0px');
      await page.reload();
      if(touch) await row.locator('.download-title').tap();
      else await row.locator('.download-open').press('Space');
      await expect.poll(()=>page.locator('#video').evaluate(video=>video.currentTime)).toBe(36);
    });
  });
}

test('a cached preview loads without preparation and missing previews remain playable', async ({page})=>{
  const row=await openPlayer(page);
  await page.goto('about:blank');
  let imageRequests=0;
  await page.route('**/api/hls/*/thumbnail-v2.jpg*',route=>{
    imageRequests++;
    return route.request().url().includes('/ready_') ? route.fulfill({contentType:'image/svg+xml',body:preview}) : route.fulfill({status:404});
  });
  await page.goto('http://cinemator.test/');
  await expect(row.locator('.download-preview')).toHaveClass(/loaded/);
  expect(await row.locator('.download-preview').evaluate(img=>img.naturalWidth)).toBe(3200);
  const other=page.locator('.download-row[data-id="other"]');
  await expect(other.locator('.download-play-icon')).toBeVisible();
  await expect(other.locator('.download-preview')).not.toHaveClass(/loaded/);
  await expect.poll(()=>imageRequests).toBe(2);
  for(let i=0;i<3;i++) await page.evaluate(()=>window.dispatchEvent(new Event('focus')));
  await expect(row.locator('.download-title')).toHaveText('Two episodes');
  expect(imageRequests).toBe(2);
  await other.locator('.download-thumbnail').click();
  await expect.poll(()=>page.locator('#video').evaluate(video=>video.duration)).toBe(120);
});

test('actions keep playback closed, support keyboard, and survive a catalog refresh',async({page})=>{
  const row=await openPlayer(page);
  const more=row.locator('.download-more');
  await more.press('Enter');
  await expect(row.locator('[data-action="restart"]')).toBeFocused();
  await page.keyboard.press('ArrowDown');
  await expect(row.locator('[data-days="1"]')).toBeFocused();
  await page.evaluate(()=>window.dispatchEvent(new Event('focus')));
  await expect(row.locator('[data-days="1"]')).toBeFocused();
  await page.keyboard.press('End');
  await expect(row.locator('[data-action="delete"]')).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(more).toBeFocused();
  await expect(page.locator('#playerWorkspace')).toBeHidden();
  await more.click();
  await page.locator('.download-row[data-id="other"] .download-more').click();
  await expect(page.locator('.download-options:visible')).toHaveCount(1);
  await expect(row.locator('.download-options')).toBeHidden();
  await page.locator('.brand').click();
  await expect(page.locator('.download-options:visible')).toHaveCount(0);
});

test('an unfinished HLS playlist is not presented as the full video duration',async({page})=>{
  const row=await openPlayer(page);
  const download={id:'ready',title:'Preparing video',magnet:'magnet:preparing',status:'preparing',
    selectedFileIndex:4,files:[{index:4,name:'Video.mkv',size:1024}]};
  await page.route(/\/api\/downloads(?:\?|$)/,route=>route.fulfill({json:[download]}));
  await page.evaluate(()=>window.dispatchEvent(new Event('focus')));
  await expect(row.locator('.download-title')).toHaveText('Preparing video');
  await row.locator('.download-open').click();
  await expect.poll(()=>page.locator('#video').evaluate(video=>video.duration)).toBe(120);
  await page.locator('#video').evaluate(video=>{video.currentTime=36;});
  await expect(row.locator('.download-resume')).toHaveText('Continue from 0:36');
  await expect(row.locator('.download-duration')).toBeHidden();
  await expect(row.locator('.download-watched')).toBeHidden();
  download.status='ready';
  await page.evaluate(()=>window.dispatchEvent(new Event('focus')));
  await expect(row).toHaveAttribute('data-status','ready');
  await page.locator('#video').evaluate(video=>{video.currentTime=37;});
  await expect(row.locator('.download-duration')).toHaveText('2:00');
  await expect(row.locator('.download-watched')).toBeVisible();
});

test('storage extension and deletion use the menu without launching a video',async({page})=>{
  const row=await openPlayer(page);
  let extension;
  await page.route('**/api/downloads/ready/extend',route=>{
    extension=route.request().postDataJSON();return route.fulfill({json:{}});
  });
  await row.locator('.download-more').click();
  await row.locator('[data-days="7"]').click();
  await expect.poll(()=>extension).toEqual({days:7});
  await expect(row.locator('.download-more')).toBeFocused();
  let removed=false;
  await page.route('**/api/downloads/ready',route=>{removed=route.request().method()==='DELETE';return route.fulfill({status:204});});
  await page.route(/\/api\/downloads(?:\?|$)/,route=>removed?route.fulfill({json:[]}):route.fallback());
  page.once('dialog',dialog=>dialog.dismiss());
  await row.locator('.download-more').click();
  await row.locator('[data-action="delete"]').click();
  expect(removed).toBe(false);
  await expect(row.locator('.download-more')).toBeFocused();
  page.once('dialog',dialog=>dialog.accept());
  await row.locator('.download-more').click();
  await row.locator('[data-action="delete"]').click();
  await expect(page.locator('.downloads-empty')).toBeVisible();
  await expect(page.locator('#addDownloadBtn')).toBeFocused();
  await expect(page.locator('#playerWorkspace')).toBeHidden();
});

for(const language of ['en','ru','zh-Hans','zh-Hant']){
  test(`${language}: action menu fits the viewport with large text and at page edges`,async({page})=>{
    const row=await openPlayer(page,{language});
    for(const fontSize of [16,32]){
      await page.evaluate(size=>document.documentElement.style.fontSize=`${size}px`,fontSize);
      for(const width of [320,390,1280]){
        await page.setViewportSize({width,height:650});
        await row.locator('.download-more').click();
        const popup=row.locator('.download-options');
        await expect(popup).toBeVisible();
        const box=await popup.boundingBox();
        expect(box.x).toBeGreaterThanOrEqual(0);
        expect(box.y).toBeGreaterThanOrEqual(0);
        expect(box.x+box.width).toBeLessThanOrEqual(width);
        expect(box.y+box.height).toBeLessThanOrEqual(650);
        expect(await popup.evaluate(el=>el.scrollWidth<=el.clientWidth+1)).toBe(true);
        await page.keyboard.press('End');
        await expect(row.locator('[data-action="delete"]')).toBeInViewport();
        await page.keyboard.press('Escape');
        await expect(row.locator('.download-more')).toBeFocused();
      }
    }
  });
}


test('five preview frames cycle automatically and respect reduced motion',async({page})=>{
 const row=await openPlayer(page);
 await page.route('**/api/hls/*/thumbnail-v2.jpg*',r=>r.fulfill({contentType:'image/svg+xml',body:preview}));
 await page.reload();
 const image=row.locator('.download-preview');
 await expect(image).toHaveClass(/loaded/);
 const frameOffset=()=>image.evaluate(img=>{
  const box=img.getBoundingClientRect(),tile=img.parentElement.getBoundingClientRect();
  return Math.round((tile.left-box.left)/tile.width);
 });
 await expect.poll(()=>image.evaluate(img=>img.getAnimations().length)).toBe(1);
 const seen=[];
 for(let step=0;step<5;step++){
  await image.evaluate((img,time)=>{const animation=img.getAnimations()[0];animation.pause();animation.currentTime=time;},step*2000+100);
  const frame=(step+2)%5;
  await expect.poll(frameOffset).toBe(frame);
  seen.push(await frameOffset());
 }
 expect(new Set(seen).size).toBe(5);
 await page.emulateMedia({reducedMotion:'reduce'});
 expect(await image.evaluate(img=>img.getAnimations().length)).toBe(0);
 expect(await frameOffset()).toBe(2);
});

test('unfinished downloads keep the default cover until HLS is ready',async({page})=>{
 const row=await openPlayer(page);
 await page.goto('about:blank');
 const download={id:'ready',title:'Preparing video',magnet:'magnet:preparing',status:'preparing',
  selectedFileIndex:4,files:[{index:4,name:'Video.mkv',size:1024}]};
 let requests=0;
 await page.route(/\/api\/downloads(?:\?|$)/,r=>r.fulfill({json:[download]}));
 await page.route('**/api/hls/*/thumbnail-v2.jpg*',r=>{requests++;return r.fulfill({contentType:'image/svg+xml',body:preview})});
 await page.goto('http://cinemator.test/');
 await expect(row).toHaveAttribute('data-status','preparing');
 const image=row.locator('.download-preview');
 await expect(image).not.toHaveAttribute('src',/.+/);
 await expect(row.locator('.download-play-icon')).toBeVisible();
 expect(requests).toBe(0);
 download.status='ready';
 await page.evaluate(()=>window.dispatchEvent(new Event('focus')));
 await expect(image).toHaveClass(/loaded/);
 expect(requests).toBe(1);
 download.status='preparing';
 await page.evaluate(()=>window.dispatchEvent(new Event('focus')));
 await expect(image).not.toHaveClass(/loaded/);
 await expect(image).not.toHaveAttribute('src',/.+/);
 await expect(row.locator('.download-play-icon')).toBeVisible();
});

test.describe('touch previews',()=>{
 test.use({hasTouch:true,isMobile:true,viewport:{width:390,height:844}});
 test('previews cycle on a phone and a tap still plays the video',async({page})=>{
  const row=await openPlayer(page);
  await page.route('**/api/hls/*/thumbnail-v2.jpg*',r=>r.fulfill({contentType:'image/svg+xml',body:preview}));
  await page.reload();
  await expect(row.locator('.download-preview')).toHaveClass(/loaded/);
  await row.locator('.download-thumbnail').tap();
  await expect.poll(()=>page.locator('#video').evaluate(video=>video.duration)).toBe(120);
  expect(await row.locator('.download-preview').evaluate(img=>img.getAnimations().length)).toBe(1);
 });
});
