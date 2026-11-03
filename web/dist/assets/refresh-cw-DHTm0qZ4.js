import{$t as e,C as t,Dn as n,F as r,Fn as i,Hn as a,Kn as o,Pn as s,Qt as c,R as l,S as u,U as d,Wn as f,_n as p,_t as m,at as h,bt as g,dt as _,ht as v,in as y,nn as b,o as x,r as S,rn as C,rt as w,st as T,tn as E,tt as D,vn as O,wn as k,xn as A,xt as j,yn as M,yt as N}from"./index-DEQQ6obZ.js";var P={closeIconSizeTiny:`12px`,closeIconSizeSmall:`12px`,closeIconSizeMedium:`14px`,closeIconSizeLarge:`14px`,closeSizeTiny:`16px`,closeSizeSmall:`16px`,closeSizeMedium:`18px`,closeSizeLarge:`18px`,padding:`0 7px`,closeMargin:`0 0 0 4px`};function F(e){let{textColor2:t,primaryColorHover:n,primaryColorPressed:r,primaryColor:i,infoColor:a,successColor:o,warningColor:s,errorColor:c,baseColor:l,borderColor:u,opacityDisabled:d,tagColor:f,closeIconColor:p,closeIconColorHover:m,closeIconColorPressed:h,borderRadiusSmall:g,fontSizeMini:_,fontSizeTiny:v,fontSizeSmall:y,fontSizeMedium:b,heightMini:x,heightTiny:S,heightSmall:C,heightMedium:w,closeColorHover:E,closeColorPressed:D,buttonColor2Hover:O,buttonColor2Pressed:k,fontWeightStrong:A}=e;return{...P,closeBorderRadius:g,heightTiny:x,heightSmall:S,heightMedium:C,heightLarge:w,borderRadius:g,opacityDisabled:d,fontSizeTiny:_,fontSizeSmall:v,fontSizeMedium:y,fontSizeLarge:b,fontWeightStrong:A,textColorCheckable:t,textColorHoverCheckable:t,textColorPressedCheckable:t,textColorChecked:l,colorCheckable:`#0000`,colorHoverCheckable:O,colorPressedCheckable:k,colorChecked:i,colorCheckedHover:n,colorCheckedPressed:r,border:`1px solid ${u}`,textColor:t,color:f,colorBordered:`rgb(250, 250, 252)`,closeIconColor:p,closeIconColorHover:m,closeIconColorPressed:h,closeColorHover:E,closeColorPressed:D,borderPrimary:`1px solid ${T(i,{alpha:.3})}`,textColorPrimary:i,colorPrimary:T(i,{alpha:.12}),colorBorderedPrimary:T(i,{alpha:.1}),closeIconColorPrimary:i,closeIconColorHoverPrimary:i,closeIconColorPressedPrimary:i,closeColorHoverPrimary:T(i,{alpha:.12}),closeColorPressedPrimary:T(i,{alpha:.18}),borderInfo:`1px solid ${T(a,{alpha:.3})}`,textColorInfo:a,colorInfo:T(a,{alpha:.12}),colorBorderedInfo:T(a,{alpha:.1}),closeIconColorInfo:a,closeIconColorHoverInfo:a,closeIconColorPressedInfo:a,closeColorHoverInfo:T(a,{alpha:.12}),closeColorPressedInfo:T(a,{alpha:.18}),borderSuccess:`1px solid ${T(o,{alpha:.3})}`,textColorSuccess:o,colorSuccess:T(o,{alpha:.12}),colorBorderedSuccess:T(o,{alpha:.1}),closeIconColorSuccess:o,closeIconColorHoverSuccess:o,closeIconColorPressedSuccess:o,closeColorHoverSuccess:T(o,{alpha:.12}),closeColorPressedSuccess:T(o,{alpha:.18}),borderWarning:`1px solid ${T(s,{alpha:.35})}`,textColorWarning:s,colorWarning:T(s,{alpha:.15}),colorBorderedWarning:T(s,{alpha:.12}),closeIconColorWarning:s,closeIconColorHoverWarning:s,closeIconColorPressedWarning:s,closeColorHoverWarning:T(s,{alpha:.12}),closeColorPressedWarning:T(s,{alpha:.18}),borderError:`1px solid ${T(c,{alpha:.23})}`,textColorError:c,colorError:T(c,{alpha:.1}),colorBorderedError:T(c,{alpha:.08}),closeIconColorError:c,closeIconColorHoverError:c,closeIconColorPressedError:c,closeColorHoverError:T(c,{alpha:.12}),closeColorPressedError:T(c,{alpha:.18})}}var I={name:`Tag`,common:h,self:F},L={color:Object,type:{type:String,default:`default`},round:Boolean,size:String,closable:Boolean,disabled:{type:Boolean,default:void 0}},R=e(`tag`,`
 --n-close-margin: var(--n-close-margin-top) var(--n-close-margin-right) var(--n-close-margin-bottom) var(--n-close-margin-left);
 white-space: nowrap;
 position: relative;
 box-sizing: border-box;
 cursor: default;
 display: inline-flex;
 align-items: center;
 flex-wrap: nowrap;
 padding: var(--n-padding);
 border-radius: var(--n-border-radius);
 color: var(--n-text-color);
 background-color: var(--n-color);
 transition: 
 border-color .3s var(--n-bezier),
 background-color .3s var(--n-bezier),
 color .3s var(--n-bezier),
 box-shadow .3s var(--n-bezier),
 opacity .3s var(--n-bezier);
 line-height: 1;
 height: var(--n-height);
 font-size: var(--n-font-size);
`,[b(`strong`,`
 font-weight: var(--n-font-weight-strong);
 `),E(`border`,`
 pointer-events: none;
 position: absolute;
 left: 0;
 right: 0;
 top: 0;
 bottom: 0;
 border-radius: inherit;
 border: var(--n-border);
 transition: border-color .3s var(--n-bezier);
 `),E(`icon`,`
 display: flex;
 margin: 0 4px 0 0;
 color: var(--n-text-color);
 transition: color .3s var(--n-bezier);
 font-size: var(--n-avatar-size-override);
 `),E(`avatar`,`
 display: flex;
 margin: 0 6px 0 0;
 `),E(`close`,`
 margin: var(--n-close-margin);
 transition:
 background-color .3s var(--n-bezier),
 color .3s var(--n-bezier);
 `),b(`round`,`
 padding: 0 calc(var(--n-height) / 3);
 border-radius: calc(var(--n-height) / 2);
 `,[E(`icon`,`
 margin: 0 4px 0 calc((var(--n-height) - 8px) / -2);
 `),E(`avatar`,`
 margin: 0 6px 0 calc((var(--n-height) - 8px) / -2);
 `),b(`closable`,`
 padding: 0 calc(var(--n-height) / 4) 0 calc(var(--n-height) / 3);
 `)]),b(`icon, avatar`,[b(`round`,`
 padding: 0 calc(var(--n-height) / 3) 0 calc(var(--n-height) / 2);
 `)]),b(`disabled`,`
 cursor: not-allowed !important;
 opacity: var(--n-opacity-disabled);
 `),b(`checkable`,`
 cursor: pointer;
 box-shadow: none;
 color: var(--n-text-color-checkable);
 background-color: var(--n-color-checkable);
 `,[C(`disabled`,[c(`&:hover`,`background-color: var(--n-color-hover-checkable);`,[C(`checked`,`color: var(--n-text-color-hover-checkable);`)]),c(`&:active`,`background-color: var(--n-color-pressed-checkable);`,[C(`checked`,`color: var(--n-text-color-pressed-checkable);`)])]),b(`checked`,`
 color: var(--n-text-color-checked);
 background-color: var(--n-color-checked);
 `,[C(`disabled`,[c(`&:hover`,`background-color: var(--n-color-checked-hover);`),c(`&:active`,`background-color: var(--n-color-checked-pressed);`)])])])]),z=[`onClick`,`onMouseenter`,`onMouseleave`],B={...D.props,...L,bordered:{type:Boolean,default:void 0},checked:Boolean,checkable:Boolean,strong:Boolean,triggerClickOnClose:Boolean,onClose:[Array,Function],onMouseenter:Function,onMouseleave:Function,"onUpdate:checked":Function,onUpdateChecked:Function,internalCloseFocusable:{type:Boolean,default:!0},internalCloseIsButtonTag:{type:Boolean,default:!0},onCheckedChange:Function},V=g(`n-tag`),H=k({name:`Tag`,props:B,slots:Object,setup(e){let n=a(null),{mergedBorderedRef:o,mergedClsPrefixRef:s,inlineThemeDisabled:c,mergedRtlRef:l,mergedComponentPropsRef:u}=N(e),m=p(()=>e.size||u?.value?.Tag?.size||`medium`),h=D(`Tag`,`-tag`,R,I,e,s);i(V,{roundRef:f(e,`round`)});function g(){if(!e.disabled&&e.checkable){let{checked:t,onCheckedChange:n,onUpdateChecked:r,"onUpdate:checked":i}=e;r&&r(!t),i&&i(!t),n&&n(!t)}}function v(t){if(e.triggerClickOnClose||t.stopPropagation(),!e.disabled){let{onClose:n}=e;n&&d(n,t)}}let b={setTextContent(e){let{value:t}=n;t&&(t.textContent=e)}},x=r(`Tag`,l,s),S=p(()=>{let{type:t,color:{color:n,textColor:r}={}}=e,i=m.value,{common:{cubicBezierEaseInOut:a},self:{padding:s,closeMargin:c,borderRadius:l,opacityDisabled:u,textColorCheckable:d,textColorHoverCheckable:f,textColorPressedCheckable:p,textColorChecked:g,colorCheckable:v,colorHoverCheckable:b,colorPressedCheckable:x,colorChecked:S,colorCheckedHover:C,colorCheckedPressed:w,closeBorderRadius:T,fontWeightStrong:E,[y(`colorBordered`,t)]:D,[y(`closeSize`,i)]:O,[y(`closeIconSize`,i)]:k,[y(`fontSize`,i)]:A,[y(`height`,i)]:j,[y(`color`,t)]:M,[y(`textColor`,t)]:N,[y(`border`,t)]:P,[y(`closeIconColor`,t)]:F,[y(`closeIconColorHover`,t)]:I,[y(`closeIconColorPressed`,t)]:L,[y(`closeColorHover`,t)]:R,[y(`closeColorPressed`,t)]:z}}=h.value,B=_(c);return{"--n-font-weight-strong":E,"--n-avatar-size-override":`calc(${j} - 8px)`,"--n-bezier":a,"--n-border-radius":l,"--n-border":P,"--n-close-icon-size":k,"--n-close-color-pressed":z,"--n-close-color-hover":R,"--n-close-border-radius":T,"--n-close-icon-color":F,"--n-close-icon-color-hover":I,"--n-close-icon-color-pressed":L,"--n-close-icon-color-disabled":F,"--n-close-margin-top":B.top,"--n-close-margin-right":B.right,"--n-close-margin-bottom":B.bottom,"--n-close-margin-left":B.left,"--n-close-size":O,"--n-color":n||(o.value?D:M),"--n-color-checkable":v,"--n-color-checked":S,"--n-color-checked-hover":C,"--n-color-checked-pressed":w,"--n-color-hover-checkable":b,"--n-color-pressed-checkable":x,"--n-font-size":A,"--n-height":j,"--n-opacity-disabled":u,"--n-padding":s,"--n-text-color":r||N,"--n-text-color-checkable":d,"--n-text-color-checked":g,"--n-text-color-hover-checkable":f,"--n-text-color-pressed-checkable":p}}),C=c?w(`tag`,p(()=>{let n=``,{type:r,color:{color:i,textColor:a}={}}=e;return n+=r[0],n+=m.value[0],i&&(n+=`a${t(i)}`),a&&(n+=`b${t(a)}`),o.value&&(n+=`c`),n}),S,e):void 0;return{...b,rtlEnabled:x,mergedClsPrefix:s,contentRef:n,mergedBordered:o,handleClick:g,handleCloseClick:v,cssVars:c?void 0:S,themeClass:C?.themeClass,onRender:C?.onRender}},render(){let{mergedClsPrefix:e,rtlEnabled:t,closable:n,color:{borderColor:r}={},round:i,onRender:a,$slots:c}=this;a?.();let d=l(c.avatar,t=>t&&(s(),A(`div`,{class:v(`${e}-tag__avatar`)},[m(()=>t)],2))),f=l(c.icon,t=>t&&(s(),A(`div`,{class:v(`${e}-tag__icon`)},[m(()=>t)],2)));return s(),A(`div`,{class:v([`${e}-tag`,this.themeClass,{[`${e}-tag--rtl`]:t,[`${e}-tag--strong`]:this.strong,[`${e}-tag--disabled`]:this.disabled,[`${e}-tag--checkable`]:this.checkable,[`${e}-tag--checked`]:this.checkable&&this.checked,[`${e}-tag--round`]:i,[`${e}-tag--avatar`]:d,[`${e}-tag--icon`]:f,[`${e}-tag--closable`]:n}]),style:o(this.cssVars),onClick:this.handleClick,onMouseenter:this.onMouseenter,onMouseleave:this.onMouseleave},[m(()=>f||d),O(`span`,{class:v(`${e}-tag__content`),ref:`contentRef`},[m(()=>this.$slots.default?.())],2),!this.checkable&&n?(s(),M(u,{key:0,clsPrefix:e,class:v(`${e}-tag__close`),disabled:this.disabled,onClick:this.handleCloseClick,focusable:this.internalCloseFocusable,round:i,isButtonTag:this.internalCloseIsButtonTag,absolute:!0},null,8,[`clsPrefix`,`class`,`disabled`,`onClick`,`focusable`,`round`,`isButtonTag`])):m(()=>null),!this.checkable&&this.mergedBordered?(s(),A(`div`,{key:2,class:v(`${e}-tag__border`),style:o({borderColor:r})},null,6)):m(()=>null)],46,z)}});function U(){let e=n(x,null);return e===null&&j(`use-dialog`,`No outer <n-dialog-provider /> founded.`),e}var W=S({name:`refresh-cw`,size:24,node:[[`path`,{d:`M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8`,key:`v9h5vc`}],[`path`,{d:`M21 3v5h-5`,key:`1q7to0`}],[`path`,{d:`M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16`,key:`3uifl3`}],[`path`,{d:`M8 16H3v5`,key:`1cv678`}]]});export{I as a,L as i,U as n,H as r,W as t};